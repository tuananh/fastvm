import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { Sandbox } from "../lib/types";
import { ThemeProvider } from "../theme/ThemeProvider";
import { useSandboxLogs } from "../lib/queries";
import { LogsDialog } from "./LogsDialog";
import { installDomStubs } from "./test-dom";

vi.mock("../lib/queries", () => ({
  useSandboxLogs: vi.fn(),
}));

const sandbox: Sandbox = {
  id: "sandbox-1",
  name: "quiet-forest-1234",
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

beforeEach(() => {
  installDomStubs();
  vi.mocked(useSandboxLogs).mockReturnValue({
    data: "boot line\n",
    isPending: false,
    isError: false,
    error: null,
    isFetching: false,
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useSandboxLogs>);
});

afterEach(cleanup);

describe("LogsDialog", () => {
  it("shows terminal-styled logs and switches sources", () => {
    render(
      <ThemeProvider>
        <LogsDialog sandbox={sandbox} open onClose={vi.fn()} />
      </ThemeProvider>,
    );

    expect(screen.getByText("boot line")).toBeInTheDocument();
    expect(screen.getByText(/PTY sessions are not logged/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "Console" }));
    expect(useSandboxLogs).toHaveBeenLastCalledWith(sandbox.id, "console", true);
  });

  it("closes from the footer", () => {
    const onClose = vi.fn();
    render(
      <ThemeProvider>
        <LogsDialog sandbox={sandbox} open onClose={onClose} />
      </ThemeProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalled();
  });
});
