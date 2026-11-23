import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { Sandbox } from "../lib/types";
import { VmDetailDrawer } from "./VmDetailDrawer";
import { installDomStubs } from "./test-dom";

vi.mock("../lib/queries", () => ({
  useSandboxStats: () => ({
    data: {
      sandbox_id: "sandbox-1",
      name: "web-1",
      state: "Running",
      cpu_percent: 10,
      vcpus: 2,
      memory_mb: 512,
      memory_rss_bytes: 1024 * 1024,
      uptime_seconds: 60,
      wake_count: 0,
      block_read_bytes: 0,
      block_write_bytes: 0,
      rx_bytes: 0,
      tx_bytes: 0,
      captured_at: "2024-06-15T12:34:56Z",
    },
    isPending: false,
    isError: false,
    error: null,
  }),
  useEvents: () => ({ data: [], isPending: false, isError: false, error: null }),
}));

beforeEach(() => {
  installDomStubs();
});

afterEach(cleanup);

const baseSandbox: Sandbox = {
  id: "sandbox-1",
  name: "web-1",
  state: "Running",
  base_disk: "sha256:image",
  vcpus: 2,
  memory_mb: 512,
  wake_count: 0,
  no_auto_sleep: false,
  last_activity_at: "2024-06-15T12:34:56Z",
  created_at: "2024-06-15T12:34:56Z",
  updated_at: "2024-06-15T12:34:56Z",
};

function renderDrawer(sandbox: Sandbox) {
  return render(
    <VmDetailDrawer
      sandbox={sandbox}
      open
      images={[]}
      pendingAction={null}
      onClose={vi.fn()}
      onAction={vi.fn()}
      onExec={vi.fn()}
    />,
  );
}

describe("VmDetailDrawer host volumes", () => {
  it("shows host → guest mounts with read-only/read-write badges", async () => {
    renderDrawer({
      ...baseSandbox,
      volumes: [
        { host_path: "/srv/data", guest_path: "/mnt/data", read_only: true },
        { host_path: "/srv/cache", guest_path: "/mnt/cache" },
      ],
    });

    expect(await screen.findByText("Host volumes")).toBeInTheDocument();
    expect(screen.getByText("/srv/data")).toBeInTheDocument();
    expect(screen.getByText("/mnt/data")).toBeInTheDocument();
    expect(screen.getByText("/srv/cache")).toBeInTheDocument();
    expect(screen.getByText("/mnt/cache")).toBeInTheDocument();
    expect(screen.getByText("read-only")).toBeInTheDocument();
    expect(screen.getByText("read-write")).toBeInTheDocument();
  });

  it("shows a graceful empty state when no volumes are attached", async () => {
    renderDrawer(baseSandbox);

    expect(await screen.findByText("Host volumes")).toBeInTheDocument();
    expect(screen.getByText("No host volumes mounted.")).toBeInTheDocument();
  });
});

describe("VmDetailDrawer data volumes", () => {
  it("shows block volume sizes and mount paths", async () => {
    renderDrawer({
      ...baseSandbox,
      data_volumes: [{ size_gb: 8, mount: "/data" }],
    });

    expect(await screen.findByText("Data volumes")).toBeInTheDocument();
    expect(screen.getByText("8 GiB")).toBeInTheDocument();
    expect(screen.getByText("/data")).toBeInTheDocument();
    expect(screen.getByText("block")).toBeInTheDocument();
  });

  it("shows a graceful empty state when no data volumes are attached", async () => {
    renderDrawer(baseSandbox);

    expect(await screen.findByText("Data volumes")).toBeInTheDocument();
    expect(screen.getByText("No data volumes attached.")).toBeInTheDocument();
  });
});
