// SPDX-License-Identifier: Apache-2.0
//
// fastvm L2 switch data plane (docs/DESIGN.md §6.6).
//
// One tc clsact ingress program is attached to every switch port (each
// sandbox's host-side veth and the uplink). Frames are forwarded with
// bpf_redirect() using a static destination-MAC -> port table. The program
//
//   - forwards frames between ports (MAC table is static: guest MACs are known),
//   - drops sandbox-to-sandbox frames by default (isolation),
//   - drops frames whose source MAC is registered to a different port
//     (L2 anti-spoofing),
//   - counts per-port rx/tx bytes/packets, drops and activity/keepalive frames,
//   - emits a ringbuf wake event when a qualifying unicast frame targets a
//     port whose "asleep" flag is set, and still redirects the frame into that
//     port so it lands in the guest's live virtio-net queue.
//
// Keepalive awareness mirrors the userspace classifier in internal/network:
// pure ACKs, zero-payload datagrams and ARP/NDP are not activity and do not
// wake; SYN/FIN/RST, TCP/UDP payload and ICMP do.
//
// Build with `make -C bpf`; the resulting object is embedded by
// internal/switch/bpf.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/icmp.h>
#include <linux/in.h>

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
#include <stdint.h>

#ifndef IP_OFFSET
#define IP_OFFSET 0x1FFF
#endif

#define MAX_MACS 4096
#define MAX_PORTS 1024
#define WAKE_RINGBUF_BYTES (1 << 16)

/* mac_key is 8 bytes so the hash map key is naturally aligned. */
struct mac_key {
	__u8 addr[6];
	__u16 pad;
};

struct port_cfg {
	__u8 is_sandbox;
	__u8 asleep;
	__u16 pad;
};

/* Per-port counters. Updated with atomic adds from the packet path. */
struct port_stats {
	__u64 rx_packets;
	__u64 rx_bytes;
	__u64 tx_packets;
	__u64 tx_bytes;
	__u64 drops;
	__u64 activity_packets;
	__u64 keepalive_packets;
	__u64 last_activity_ns;
};

struct global_cfg {
	__u32 uplink_ifindex;
	__u8 isolate;
	__u8 wake_notify;
	__u16 pad;
};

struct wake_event {
	__u32 ifindex;
	__u32 pad;
	__u64 ts_ns;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_MACS);
	__type(key, struct mac_key);
	__type(value, __u32);
} mac_to_port SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_PORTS);
	__type(key, __u32);
	__type(value, struct port_cfg);
} port_cfg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_PORTS);
	__type(key, __u32);
	__type(value, struct port_stats);
} port_stats SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct global_cfg);
} global_cfg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, WAKE_RINGBUF_BYTES);
} wake_events SEC(".maps");

char _license[] SEC("license") = "GPL";

/* Traffic classification result. */
enum {
	CLASS_NOT_WAKE = 0, /* ARP/NDP/non-IPv4: forward, never wake */
	CLASS_KEEPALIVE = 1, /* pure ACK / empty payload: no activity, no wake */
	CLASS_ACTIVITY = 2, /* data, SYN/FIN/RST, UDP/ICMP: activity + wake */
};

static __always_inline void mac_key_from(const __u8 *src, struct mac_key *key)
{
	__builtin_memcpy(key->addr, src, 6);
	key->pad = 0;
}

static __always_inline void stats_count(struct port_stats *st, __u64 bytes,
					int rx)
{
	if (!st)
		return;

	if (rx) {
		__sync_fetch_and_add(&st->rx_packets, 1);
		__sync_fetch_and_add(&st->rx_bytes, bytes);
	} else {
		__sync_fetch_and_add(&st->tx_packets, 1);
		__sync_fetch_and_add(&st->tx_bytes, bytes);
	}
}

static __always_inline void stats_activity(struct port_stats *st, int klass)
{
	if (!st)
		return;

	__u64 now = bpf_ktime_get_ns();
	if (klass == CLASS_ACTIVITY) {
		__sync_fetch_and_add(&st->activity_packets, 1);
		st->last_activity_ns = now;
	} else if (klass == CLASS_KEEPALIVE) {
		__sync_fetch_and_add(&st->keepalive_packets, 1);
	}
}

/*
 * classify_l4 returns CLASS_ACTIVITY for TCP data/SYN/FIN/RST, UDP datagrams
 * with a payload and ICMP; CLASS_KEEPALIVE for pure ACKs and zero-payload
 * datagrams; CLASS_NOT_WAKE for anything we cannot classify (IPv6, fragments,
 * non-IP). Only CLASS_ACTIVITY counts as activity and can trigger a wake.
 */
static __always_inline int classify_l4(struct ethhdr *eth, void *data_end,
				       __u32 pkt_len)
{
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return CLASS_NOT_WAKE;

	struct iphdr *ip = (void *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return CLASS_NOT_WAKE;

	__u32 ihl = (__u32)ip->ihl * 4;
	if (ihl < sizeof(struct iphdr))
		return CLASS_NOT_WAKE;
	if ((void *)ip + ihl > data_end)
		return CLASS_NOT_WAKE;

	/* Non-first fragments carry no L4 header; treat as data in flight. */
	if (ip->frag_off & bpf_htons(IP_OFFSET))
		return CLASS_ACTIVITY;

	__u32 l4_off = sizeof(struct ethhdr) + ihl;

	if (ip->protocol == IPPROTO_TCP) {
		struct tcphdr *tcp = (void *)ip + ihl;
		if ((void *)(tcp + 1) > data_end)
			return CLASS_KEEPALIVE;

		__u16 flags = tcp->syn | tcp->fin | tcp->rst;
		int has_flags = tcp->syn || tcp->fin || tcp->rst;
		(void)flags;

		__u32 doff = (__u32)tcp->doff * 4;
		if (doff < sizeof(struct tcphdr))
			return CLASS_KEEPALIVE;
		if (l4_off + doff > pkt_len)
			return CLASS_KEEPALIVE;

		__u32 payload = pkt_len - l4_off - doff;
		if (has_flags || payload > 0)
			return CLASS_ACTIVITY;
		return CLASS_KEEPALIVE;
	}

	if (ip->protocol == IPPROTO_UDP) {
		struct udphdr *udp = (void *)ip + ihl;
		if ((void *)(udp + 1) > data_end)
			return CLASS_KEEPALIVE;

		__u32 ulen = bpf_ntohs(udp->len);
		if (ulen > sizeof(struct udphdr))
			return CLASS_ACTIVITY;
		return CLASS_KEEPALIVE;
	}

	if (ip->protocol == IPPROTO_ICMP)
		return CLASS_ACTIVITY;

	return CLASS_KEEPALIVE;
}

SEC("tc")
int l2switch(struct __sk_buff *skb)
{
	__u32 zero = 0;
	struct global_cfg *cfg = bpf_map_lookup_elem(&global_cfg, &zero);
	if (!cfg)
		return TC_ACT_OK;

	__u32 ingress = skb->ifindex;
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;

	struct port_cfg *ingress_cfg = bpf_map_lookup_elem(&port_cfg, &ingress);
	struct port_stats *ingress_stats = bpf_map_lookup_elem(&port_stats,
							       &ingress);

	stats_count(ingress_stats, skb->len, 1);

	/* --- source MAC anti-spoofing ------------------------------------ */
	struct mac_key src_key = {};
	mac_key_from(eth->h_source, &src_key);
	__u32 *src_port = bpf_map_lookup_elem(&mac_to_port, &src_key);

	int ingress_is_sandbox = ingress_cfg && ingress_cfg->is_sandbox;
	if (src_port) {
		if (*src_port != ingress) {
			/* A frame claiming another port's MAC. */
			if (ingress_stats)
				__sync_fetch_and_add(&ingress_stats->drops, 1);
			return TC_ACT_SHOT;
		}
	} else if (ingress_is_sandbox) {
		/* Sandbox traffic must use its assigned MAC. */
		if (ingress_stats)
			__sync_fetch_and_add(&ingress_stats->drops, 1);
		return TC_ACT_SHOT;
	}

	/* --- destination lookup ------------------------------------------ */
	struct mac_key dst_key = {};
	mac_key_from(eth->h_dest, &dst_key);
	__u32 *dst_port = bpf_map_lookup_elem(&mac_to_port, &dst_key);

	if (!dst_port) {
		/* Unknown destination: sandbox traffic leaves via the uplink. */
		if (ingress_is_sandbox && cfg->uplink_ifindex) {
			struct port_stats *uplink_stats =
				bpf_map_lookup_elem(&port_stats,
						    &cfg->uplink_ifindex);
			stats_count(uplink_stats, skb->len, 0);
			return bpf_redirect(cfg->uplink_ifindex, 0);
		}
		return TC_ACT_OK;
	}

	__u32 dst = *dst_port;
	if (dst == ingress)
		return TC_ACT_OK;

	struct port_cfg *dst_cfg = bpf_map_lookup_elem(&port_cfg, &dst);

	/* --- default sandbox <> sandbox isolation ------------------------ */
	if (cfg->isolate && ingress_is_sandbox && dst_cfg && dst_cfg->is_sandbox) {
		struct port_stats *dst_stats = bpf_map_lookup_elem(&port_stats,
								   &dst);
		if (ingress_stats)
			__sync_fetch_and_add(&ingress_stats->drops, 1);
		if (dst_stats)
			__sync_fetch_and_add(&dst_stats->drops, 1);
		return TC_ACT_SHOT;
	}

	/* --- activity accounting ----------------------------------------- */
	int klass = classify_l4(eth, data_end, skb->len);
	if (klass != CLASS_NOT_WAKE) {
		struct port_stats *dst_stats = bpf_map_lookup_elem(&port_stats,
								   &dst);
		stats_activity(ingress_stats, klass);
		stats_activity(dst_stats, klass);
	}

	/* --- wake notification for sleeping ports ------------------------ */
	if (klass == CLASS_ACTIVITY && cfg->wake_notify && dst_cfg &&
	    dst_cfg->asleep) {
		struct wake_event ev = {
			.ifindex = dst,
			.pad = 0,
			.ts_ns = bpf_ktime_get_ns(),
		};
		if (bpf_ringbuf_output(&wake_events, &ev, sizeof(ev), 0) < 0) {
			struct port_stats *dst_stats =
				bpf_map_lookup_elem(&port_stats, &dst);
			if (dst_stats)
				__sync_fetch_and_add(&dst_stats->drops, 1);
		}
	}

	/* --- forward into the destination port --------------------------- */
	{
		struct port_stats *dst_stats = bpf_map_lookup_elem(&port_stats,
								   &dst);
		stats_count(dst_stats, skb->len, 0);
	}
	return bpf_redirect(dst, 0);
}
