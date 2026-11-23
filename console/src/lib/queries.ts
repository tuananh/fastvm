import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { QueryClient } from "@tanstack/react-query";
import {
  createSandbox,
  deleteSandbox,
  getSandbox,
  getSandboxStats,
  getVersion,
  listEvents,
  listImages,
  listSandboxes,
  pullImage,
  removeImage,
  sleepSandbox,
  wakeSandbox,
} from "./api";
import type { CreateSandboxInput, PullImageInput } from "./api";

/** Stable TanStack Query keys. Mutations invalidate by key prefix. */
export const queryKeys = {
  version: ["version"] as const,
  sandboxes: ["sandboxes"] as const,
  sandbox: (ref: string) => ["sandbox", ref] as const,
  sandboxStats: (ref: string) => ["sandbox-stats", ref] as const,
  images: ["images"] as const,
  events: ["events"] as const,
};

function invalidateSandboxQueries(queryClient: QueryClient, ref?: string): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.sandboxes });
  void queryClient.invalidateQueries({ queryKey: queryKeys.events });
  if (ref !== undefined) {
    void queryClient.invalidateQueries({ queryKey: queryKeys.sandbox(ref) });
    void queryClient.invalidateQueries({ queryKey: queryKeys.sandboxStats(ref) });
  }
}

/** fastvmd version; fetched once per page load. */
export function useVersion() {
  return useQuery({ queryKey: queryKeys.version, queryFn: getVersion });
}

/** All sandboxes, polled every 3s. */
export function useSandboxes() {
  return useQuery({
    queryKey: queryKeys.sandboxes,
    queryFn: listSandboxes,
    refetchInterval: 3_000,
  });
}

/** One sandbox by name or id; disabled while `ref` is empty. */
export function useSandbox(ref: string | undefined) {
  return useQuery({
    queryKey: queryKeys.sandbox(ref ?? ""),
    queryFn: () => getSandbox(ref ?? ""),
    enabled: Boolean(ref),
  });
}

/** Live utilization stats for one sandbox, polled every 2s. */
export function useSandboxStats(ref: string | undefined) {
  return useQuery({
    queryKey: queryKeys.sandboxStats(ref ?? ""),
    queryFn: () => getSandboxStats(ref ?? ""),
    enabled: Boolean(ref),
    refetchInterval: 2_000,
  });
}

/** Base disk images; rarely changes, cached for 60s. */
export function useImages() {
  return useQuery({
    queryKey: queryKeys.images,
    queryFn: listImages,
    staleTime: 60_000,
  });
}

/** `POST /v1/images/pull` */
export function usePullImage() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: PullImageInput) => pullImage(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.images });
    },
  });
}

export interface RemoveImageVars {
  ref: string;
  force?: boolean;
}

/** `DELETE /v1/images?ref=...` */
export function useRemoveImage() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ ref, force }: RemoveImageVars) => removeImage(ref, force),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.images });
    },
  });
}

/** Recent lifecycle events, polled every 5s. */
export function useEvents(limit = 50) {
  return useQuery({
    queryKey: [...queryKeys.events, limit] as const,
    queryFn: () => listEvents(limit),
    refetchInterval: 5_000,
  });
}

export interface SleepSandboxVars {
  ref: string;
  deep: boolean;
}

/** `POST /v1/sandboxes/{ref}/sleep` */
export function useSleepSandbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ ref, deep }: SleepSandboxVars) => sleepSandbox(ref, deep),
    onSuccess: (_data, { ref }) => {
      invalidateSandboxQueries(queryClient, ref);
    },
  });
}

/** `POST /v1/sandboxes/{ref}/wake` */
export function useWakeSandbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ref: string) => wakeSandbox(ref),
    onSuccess: (_data, ref) => {
      invalidateSandboxQueries(queryClient, ref);
    },
  });
}

/** `DELETE /v1/sandboxes/{ref}` */
export function useDeleteSandbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ref: string) => deleteSandbox(ref),
    onSuccess: (_data, ref) => {
      // The sandbox is gone: drop its cached detail/stats queries instead of
      // invalidating them, otherwise an active observer would refetch and 404.
      queryClient.removeQueries({ queryKey: queryKeys.sandbox(ref) });
      queryClient.removeQueries({ queryKey: queryKeys.sandboxStats(ref) });
      void queryClient.invalidateQueries({ queryKey: queryKeys.sandboxes });
      void queryClient.invalidateQueries({ queryKey: queryKeys.events });
    },
  });
}

/** `POST /v1/sandboxes` */
export function useCreateSandbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateSandboxInput) => createSandbox(input),
    onSuccess: (sandbox) => {
      invalidateSandboxQueries(queryClient, sandbox.id);
    },
  });
}
