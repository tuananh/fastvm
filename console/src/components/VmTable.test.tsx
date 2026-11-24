import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import type { Sandbox } from "../lib/types";
import { VmTable } from "./VmTable";

// The table intentionally renders even when live stats are unavailable; keep
// the test offline instead of touching the daemon.
vi.mock("../lib/api", () => ({
  getSandboxStats: vi.fn(async () => {
    throw new Error("offline");
  }),
}));

const now = "2026-01-01T00:00:00Z";

function sandbox(overrides: Partial<Sandbox> & Pick<Sandbox, "id" | "name" | "state">): Sandbox {
  return {
    base_disk: "sha256:0123456789abcdef0123456789abcdef",
    vcpus: 2,
    memory_mb: 512,
    wake_count: 0,
    no_auto_sleep: false,
    last_activity_at: now,
    created_at: now,
    updated_at: now,
    ...overrides,
  };
}

const SANDBOXES: readonly Sandbox[] = [
  sandbox({ id: "aaaa11112222", name: "web-frontend", state: "Running", ip: "10.77.0.2" }),
  sandbox({ id: "bbbb33334444", name: "api-worker", state: "Idle", ip: "10.77.0.3" }),
  sandbox({ id: "cccc55556666", name: "agent-sandbox", state: "Dormant", ip: "10.77.0.4" }),
];

function renderTable() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <VmTable
        sandboxes={SANDBOXES}
        images={[]}
        loading={false}
        error={null}
        pendingAction={null}
        onCreate={vi.fn()}
        onRetry={vi.fn()}
        onDetails={vi.fn()}
        onAction={vi.fn()}
        onExec={vi.fn()}
        onLogs={vi.fn()}
      />
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

describe("VmTable filters", () => {
  it("filters by name and ID text", () => {
    renderTable();
    expect(screen.getByText("web-frontend")).toBeInTheDocument();
    expect(screen.getByText("api-worker")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Filter sandboxes"), { target: { value: "worker" } });
    expect(screen.queryByText("web-frontend")).not.toBeInTheDocument();
    expect(screen.getByText("api-worker")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Filter sandboxes"), { target: { value: "cccc5555" } });
    expect(screen.getByText("agent-sandbox")).toBeInTheDocument();
    expect(screen.queryByText("api-worker")).not.toBeInTheDocument();
  });

  it("filters by lifecycle state", () => {
    renderTable();
    fireEvent.change(screen.getByLabelText("Filter by state"), { target: { value: "Dormant" } });
    expect(screen.getByText("agent-sandbox")).toBeInTheDocument();
    expect(screen.queryByText("web-frontend")).not.toBeInTheDocument();
    expect(screen.queryByText("api-worker")).not.toBeInTheDocument();
    expect(screen.getByText("1 of 3 VMs")).toBeInTheDocument();
  });

  it("shows an empty state and clears filters", () => {
    renderTable();
    fireEvent.change(screen.getByLabelText("Filter sandboxes"), { target: { value: "nope" } });
    expect(screen.getByText("No sandboxes match")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByText("web-frontend")).toBeInTheDocument();
    expect(screen.getByText("api-worker")).toBeInTheDocument();
    expect(screen.getByText("agent-sandbox")).toBeInTheDocument();
  });
});
