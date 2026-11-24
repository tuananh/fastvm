import type { Sandbox } from "../lib/types";
import { DropdownMenu } from "../ui";
import type { MenuAction } from "../ui";
import {
  canDeepSleep,
  canDelete,
  canExec,
  canPause,
  canResume,
  type PendingAction,
  type VmActionKind,
} from "./vmActions";
import {
  InfoIcon,
  ListBulletIcon,
  MoonIcon,
  MoreIcon,
  PauseIcon,
  PlayIcon,
  TerminalIcon,
  TrashIcon,
} from "./icons";

export interface VmActionsMenuProps {
  sandbox: Sandbox;
  pending: PendingAction | null;
  onAction: (kind: VmActionKind, sandbox: Sandbox) => void;
  onDetails: (sandbox: Sandbox) => void;
  /** Open an interactive shell; wakes paused/dormant sandboxes. */
  onExec: (sandbox: Sandbox) => void;
  /** Open the captured log viewer. */
  onLogs: (sandbox: Sandbox) => void;
}

/**
 * Per-row lifecycle menu. Actions are disabled when the current state does not
 * accept them (fastvmd would reject those calls anyway) and while another
 * action for the same sandbox is in flight.
 */
export function VmActionsMenu({
  sandbox,
  pending,
  onAction,
  onDetails,
  onExec,
  onLogs,
}: VmActionsMenuProps) {
  const busy = pending !== null;

  const items: MenuAction[] = [
    {
      key: "details",
      label: "Details",
      icon: <InfoIcon className="size-4" />,
      onClick: () => {
        onDetails(sandbox);
      },
    },
    {
      key: "exec",
      label: "Open terminal",
      icon: <TerminalIcon className="size-4" />,
      disabled: busy || !canExec(sandbox.state),
      onClick: () => {
        onExec(sandbox);
      },
    },
    {
      key: "logs",
      label: "View logs",
      icon: <ListBulletIcon className="size-4" />,
      onClick: () => {
        onLogs(sandbox);
      },
    },
    {
      key: "pause",
      label: "Pause",
      icon: <PauseIcon className="size-4" />,
      separatorBefore: true,
      disabled: busy || !canPause(sandbox.state),
      onClick: () => {
        onAction("pause", sandbox);
      },
    },
    {
      key: "deepSleep",
      label: "Deep sleep",
      icon: <MoonIcon className="size-4" />,
      disabled: busy || !canDeepSleep(sandbox.state),
      onClick: () => {
        onAction("deepSleep", sandbox);
      },
    },
    {
      key: "resume",
      label: "Resume",
      icon: <PlayIcon className="size-4" />,
      disabled: busy || !canResume(sandbox.state),
      onClick: () => {
        onAction("resume", sandbox);
      },
    },
    {
      key: "delete",
      label: "Delete",
      icon: <TrashIcon className="size-4" />,
      danger: true,
      separatorBefore: true,
      disabled: busy || !canDelete(sandbox.state),
      onClick: () => {
        onAction("delete", sandbox);
      },
    },
  ];

  return (
    <span
      className="inline-flex"
      onClick={(event) => {
        // The trigger lives inside a clickable table row; keep menu clicks local.
        event.stopPropagation();
      }}
    >
      <DropdownMenu
        label={`Actions for ${sandbox.name}`}
        trigger={<MoreIcon className="size-5" />}
        triggerClassName="inline-flex rounded-lg p-2 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-900 focus-visible:outline-none focus-visible:ring-4 focus-visible:ring-indigo-200 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white dark:focus-visible:ring-indigo-900"
        items={items}
        menuClassName="w-48"
        footer={
          busy ? (
            <span className="text-xs text-gray-500 dark:text-gray-400" role="status">
              Working…
            </span>
          ) : undefined
        }
      />
    </span>
  );
}

export default VmActionsMenu;
