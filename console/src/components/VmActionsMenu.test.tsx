import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import type { Sandbox } from "../lib/types";
import { VmActionsMenu } from "./VmActionsMenu";
import { installDomStubs } from "./test-dom";

afterEach(cleanup);

const sandbox: Sandbox = {
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

async function openMenu() {
  fireEvent.click(screen.getByRole("button", { name: "Actions for web-1" }));
  expect(await screen.findByRole("menu")).toBeInTheDocument();
}

describe("VmActionsMenu", () => {
  it("opens the lifecycle menu and fires the action", async () => {
    installDomStubs();
    const onAction = vi.fn();
    const onDetails = vi.fn();

    render(
      <VmActionsMenu
        sandbox={sandbox}
        pending={null}
        onAction={onAction}
        onDetails={onDetails}
        onExec={() => {}}
        onLogs={() => {}}
      />,
    );

    await openMenu();
    fireEvent.click(screen.getByRole("menuitem", { name: "Pause" }));

    expect(onAction).toHaveBeenCalledWith("pause", sandbox);
  });

  it("keeps menu clicks from bubbling to the clickable table row", async () => {
    installDomStubs();
    const rowClick = vi.fn();
    const onAction = vi.fn();

    render(
      <table>
        <tbody>
          <tr onClick={rowClick}>
            <td>
              <VmActionsMenu
                sandbox={sandbox}
                pending={null}
                onAction={onAction}
                onDetails={() => {}}
                onExec={() => {}}
                onLogs={() => {}}
              />
            </td>
          </tr>
        </tbody>
      </table>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Actions for web-1" }));
    expect(rowClick).not.toHaveBeenCalled();

    fireEvent.click(await screen.findByRole("menuitem", { name: "Pause" }));
    expect(rowClick).not.toHaveBeenCalled();
  });

  it("disables lifecycle actions that the state does not accept", async () => {
    installDomStubs();
    render(
      <VmActionsMenu
        sandbox={{ ...sandbox, state: "Paused" }}
        pending={null}
        onAction={() => {}}
        onDetails={() => {}}
        onExec={() => {}}
        onLogs={() => {}}
      />,
    );

    await openMenu();
    expect(screen.getByRole("menuitem", { name: "Pause" })).toBeDisabled();
    expect(screen.getByRole("menuitem", { name: "Deep sleep" })).toBeEnabled();
    expect(screen.getByRole("menuitem", { name: "Resume" })).toBeEnabled();
  });

  it("opens a terminal for Running sandboxes", async () => {
    installDomStubs();
    const onExec = vi.fn();

    render(
      <VmActionsMenu
        sandbox={sandbox}
        pending={null}
        onAction={() => {}}
        onDetails={() => {}}
        onExec={onExec}
        onLogs={() => {}}
      />,
    );

    await openMenu();
    const terminalItem = screen.getByRole("menuitem", { name: "Open terminal" });
    expect(terminalItem).toBeEnabled();

    fireEvent.click(terminalItem);
    expect(onExec).toHaveBeenCalledWith(sandbox);
  });

  it("opens the log viewer", async () => {
    installDomStubs();
    const onLogs = vi.fn();

    render(
      <VmActionsMenu
        sandbox={sandbox}
        pending={null}
        onAction={() => {}}
        onDetails={() => {}}
        onExec={() => {}}
        onLogs={onLogs}
      />,
    );

    await openMenu();
    fireEvent.click(screen.getByRole("menuitem", { name: "View logs" }));
    expect(onLogs).toHaveBeenCalledWith(sandbox);
  });

  it("disables the terminal action while the sandbox is still being created", async () => {
    installDomStubs();

    render(
      <VmActionsMenu
        sandbox={{ ...sandbox, state: "Creating" }}
        pending={null}
        onAction={() => {}}
        onDetails={() => {}}
        onExec={() => {}}
        onLogs={() => {}}
      />,
    );

    await openMenu();
    expect(screen.getByRole("menuitem", { name: "Open terminal" })).toBeDisabled();
  });
});
