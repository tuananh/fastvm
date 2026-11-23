import { Bars3Icon } from "@heroicons/react/24/outline";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useState } from "react";
import { AppSidebar } from "../components/AppSidebar";
import type { ConsoleView } from "../components/AppSidebar";
import { CreateVmModal } from "../components/CreateVmModal";
import { ClockIcon, PlusIcon, RefreshIcon } from "../components/icons";
import { SummaryCards } from "../components/SummaryCards";
import { TerminalDialog } from "../components/TerminalDialog";
import { VmDetailDrawer } from "../components/VmDetailDrawer";
import { VmTable } from "../components/VmTable";
import ImagesPage from "./ImagesPage";
import type { PendingAction, VmActionKind } from "../components/vmActions";
import {
  queryKeys,
  useDeleteSandbox,
  useImages,
  useSandboxes,
  useSleepSandbox,
  useVersion,
  useWakeSandbox,
} from "../lib/queries";
import type { BaseDisk, Sandbox } from "../lib/types";
import { Button, IconButton, Modal, cn, useToast } from "../ui";
import { describeError } from "../utils/errors";
import { formatClockTime, formatDateTime } from "../utils/format";

const EMPTY_SANDBOXES: readonly Sandbox[] = [];
const EMPTY_IMAGES: readonly BaseDisk[] = [];

/**
 * Main console screen: HyperUI sidebar shell with a polled sandbox table,
 * summary counters, lifecycle actions, create flow and live detail drawer.
 */
export default function DashboardPage() {
  const toast = useToast();
  const queryClient = useQueryClient();
  const sandboxesQuery = useSandboxes();
  const imagesQuery = useImages();
  const version = useVersion();
  const sleepSandbox = useSleepSandbox();
  const wakeSandbox = useWakeSandbox();
  const deleteSandbox = useDeleteSandbox();

  const [view, setView] = useState<ConsoleView>(() =>
    typeof window !== "undefined" && window.location.hash === "#images" ? "images" : "sandboxes",
  );
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [openPullOnImages, setOpenPullOnImages] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [createImage, setCreateImage] = useState<string | null>(null);
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const [pendingAction, setPendingAction] = useState<PendingAction | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Sandbox | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [detailSnapshot, setDetailSnapshot] = useState<Sandbox | null>(null);
  const [detailRef, setDetailRef] = useState<string | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [terminalTarget, setTerminalTarget] = useState<Sandbox | null>(null);
  const [terminalOpen, setTerminalOpen] = useState(false);

  useEffect(() => {
    const onHashChange = () => {
      setView(window.location.hash === "#images" ? "images" : "sandboxes");
    };
    window.addEventListener("hashchange", onHashChange);
    return () => {
      window.removeEventListener("hashchange", onHashChange);
    };
  }, []);

  const navigate = useCallback((next: ConsoleView) => {
    setView(next);
    const hash = next === "images" ? "#images" : "#sandboxes";
    if (window.location.hash !== hash) {
      window.history.replaceState(null, "", hash);
    }
  }, []);

  const openCreate = useCallback(() => {
    setCreateImage(null);
    setCreateOpen(true);
  }, []);

  const createFromImage = useCallback(
    (image: BaseDisk) => {
      setCreateImage(image.digest);
      setCreateOpen(true);
      navigate("sandboxes");
    },
    [navigate],
  );

  const sandboxes = sandboxesQuery.data ?? EMPTY_SANDBOXES;
  const images = imagesQuery.data ?? EMPTY_IMAGES;

  const detailSandbox =
    detailRef === null
      ? detailSnapshot
      : (sandboxes.find((sandbox) => sandbox.id === detailRef) ?? detailSnapshot);

  const terminalSandbox =
    terminalTarget === null
      ? null
      : (sandboxes.find((sandbox) => sandbox.id === terminalTarget.id) ?? terminalTarget);

  const openDetails = useCallback((sandbox: Sandbox) => {
    setDetailSnapshot(sandbox);
    setDetailRef(sandbox.id);
    setDrawerOpen(true);
  }, []);

  const closeDrawer = useCallback(() => {
    setDrawerOpen(false);
    setDetailRef(null);
    setDetailSnapshot(null);
  }, []);

  const openTerminal = useCallback((sandbox: Sandbox) => {
    setTerminalTarget(sandbox);
    setTerminalOpen(true);
  }, []);

  const closeTerminal = useCallback(() => {
    setTerminalOpen(false);
    setTerminalTarget(null);
  }, []);

  const handleRefresh = useCallback(async () => {
    setManualRefreshing(true);
    try {
      await sandboxesQuery.refetch();
    } finally {
      setManualRefreshing(false);
    }
  }, [sandboxesQuery]);

  const openDelete = useCallback((sandbox: Sandbox) => {
    setDeleteTarget(sandbox);
    setDeleteOpen(true);
  }, []);

  const handleAction = useCallback(
    async (kind: VmActionKind, sandbox: Sandbox) => {
      if (kind === "delete") {
        openDelete(sandbox);
        return;
      }
      if (pendingAction !== null) {
        return;
      }
      setPendingAction({ ref: sandbox.id, kind });
      try {
        if (kind === "pause") {
          await sleepSandbox.mutateAsync({ ref: sandbox.id, deep: false });
          toast.success(`${sandbox.name} paused`);
        } else if (kind === "deepSleep") {
          await sleepSandbox.mutateAsync({ ref: sandbox.id, deep: true });
          toast.success(`${sandbox.name} is deeply asleep`);
        } else {
          await wakeSandbox.mutateAsync(sandbox.id);
          toast.success(`${sandbox.name} resumed`);
        }
      } catch (error) {
        toast.error(describeError(error));
      } finally {
        setPendingAction(null);
      }
    },
    [openDelete, pendingAction, sleepSandbox, toast, wakeSandbox],
  );

  const closeDelete = useCallback(() => {
    if (pendingAction?.kind === "delete") {
      return;
    }
    setDeleteOpen(false);
    setDeleteTarget(null);
  }, [pendingAction]);

  const confirmDelete = useCallback(async () => {
    if (deleteTarget === null) {
      return;
    }
    const target = deleteTarget;
    setPendingAction({ ref: target.id, kind: "delete" });
    try {
      await deleteSandbox.mutateAsync(target.id);
      // Remove the row immediately: its stats query would otherwise keep polling a
      // sandbox that no longer exists and log a 404 until the list refetch lands.
      queryClient.setQueryData<Sandbox[]>(queryKeys.sandboxes, (previous) =>
        previous === undefined ? previous : previous.filter((sandbox) => sandbox.id !== target.id),
      );
      queryClient.removeQueries({ queryKey: queryKeys.sandboxStats(target.id) });
      toast.success(`Deleted ${target.name}`);
      setDeleteOpen(false);
      setDeleteTarget(null);
      if (detailRef === target.id) {
        closeDrawer();
      }
      if (terminalTarget?.id === target.id) {
        closeTerminal();
      }
    } catch (error) {
      toast.error(describeError(error));
    } finally {
      setPendingAction(null);
    }
  }, [
    closeDrawer,
    closeTerminal,
    deleteSandbox,
    deleteTarget,
    detailRef,
    queryClient,
    terminalTarget,
    toast,
  ]);

  const deleting = pendingAction?.kind === "delete";
  const listError = sandboxesQuery.isError ? describeError(sandboxesQuery.error) : null;
  const lastUpdated = sandboxesQuery.dataUpdatedAt;
  const lastUpdatedLabel = lastUpdated > 0 ? formatClockTime(lastUpdated) : "—";

  const versionText = version.isPending
    ? "fastvmd …"
    : version.isError
      ? "fastvmd offline"
      : version.data?.name !== undefined && version.data.name !== ""
        ? `${version.data.name} v${version.data.version}`
        : `fastvmd ${version.data?.version ?? "?"}`;

  return (
    <div className="flex h-screen overflow-hidden bg-gray-50 dark:bg-gray-950">
      <AppSidebar
        open={sidebarOpen}
        view={view}
        onNavigate={navigate}
        onClose={() => setSidebarOpen(false)}
      />

      <div className="flex flex-1 flex-col overflow-y-auto">
        <header className="flex items-center justify-between gap-4 border-b border-gray-200 bg-white px-4 py-4 sm:px-6 lg:px-8 dark:border-gray-800 dark:bg-gray-900">
          <div className="flex min-w-0 items-center gap-3">
            <button
              type="button"
              aria-label="Open sidebar"
              aria-expanded={sidebarOpen}
              aria-controls="app-sidebar"
              onClick={() => setSidebarOpen(true)}
              className="rounded-lg p-2 text-gray-600 transition-colors hover:bg-gray-100 hover:text-gray-900 focus-visible:outline-none focus-visible:ring-4 focus-visible:ring-indigo-200 lg:hidden dark:text-gray-300 dark:hover:bg-gray-800 dark:hover:text-white dark:focus-visible:ring-indigo-900"
            >
              <Bars3Icon className="size-5" />
            </button>
            <div className="min-w-0">
              <h1 className="truncate text-lg font-semibold text-gray-900 dark:text-white">
                {view === "images" ? "Images" : "Sandboxes"}
              </h1>
              <p className="truncate text-xs text-gray-500 dark:text-gray-400">
                {view === "images"
                  ? "Read-only OCI base disks shared by sandboxes."
                  : "Micro-VMs on this host. Pause, offload, or wake them — traffic brings them back."}
              </p>
            </div>
          </div>

          <div className="flex shrink-0 items-center gap-2 sm:gap-3">
            <span
              className={cn(
                "hidden items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium whitespace-nowrap sm:inline-flex",
                version.isError
                  ? "border-red-200 bg-red-50 text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300"
                  : version.isPending
                    ? "border-gray-200 bg-gray-50 text-gray-500 dark:border-gray-800 dark:bg-gray-800/50 dark:text-gray-400"
                    : "border-gray-200 bg-gray-50 text-gray-700 dark:border-gray-800 dark:bg-gray-800/50 dark:text-gray-200",
              )}
              title={
                version.isError
                  ? "fastvmd is unreachable; lifecycle data cannot be loaded."
                  : "Connected to fastvmd over the same-origin API."
              }
            >
              <span
                aria-hidden="true"
                className={cn(
                  "size-2 rounded-full",
                  version.isError
                    ? "bg-red-500"
                    : version.isPending
                      ? "bg-gray-400"
                      : "bg-green-500",
                )}
              />
              {versionText}
            </span>

            {view === "sandboxes" ? (
              <>
                <IconButton
                  label="Refresh sandboxes"
                  onClick={() => void handleRefresh()}
                  disabled={manualRefreshing}
                >
                  <RefreshIcon className={manualRefreshing ? "size-5 spinning" : "size-5"} />
                </IconButton>

                <Button
                  variant="primary"
                  icon={<PlusIcon className="size-4" />}
                  onClick={openCreate}
                >
                  Create VM
                </Button>
              </>
            ) : null}
          </div>
        </header>

        <main className="flex-1 space-y-6 p-4 sm:p-6 lg:p-8">
          {view === "images" ? (
            <ImagesPage
              autoOpenPull={openPullOnImages}
              onAutoOpenConsumed={() => setOpenPullOnImages(false)}
              onCreateVm={createFromImage}
            />
          ) : (
            <>
              <section id="overview" aria-label="Overview">
                <SummaryCards sandboxes={sandboxes} loading={sandboxesQuery.isPending} />
              </section>

              <section
                id="sandboxes"
                aria-label="Sandboxes"
                className="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-gray-800 dark:bg-gray-900"
              >
                <div className="flex flex-wrap items-center justify-between gap-3 border-b border-gray-200 px-4 py-4 sm:px-6 dark:border-gray-800">
                  <div>
                    <h2 className="text-sm font-semibold text-gray-900 dark:text-white">
                      All sandboxes
                    </h2>
                    <p className="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
                      {sandboxes.length} VM{sandboxes.length === 1 ? "" : "s"} · Updates every 3s ·
                      stats every 2s
                    </p>
                  </div>
                  <span
                    className="inline-flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400"
                    title={formatDateTime(lastUpdated)}
                  >
                    <ClockIcon className="size-3.5" /> Last updated {lastUpdatedLabel}
                  </span>
                </div>

                <VmTable
                  sandboxes={sandboxes}
                  images={images}
                  loading={sandboxesQuery.isPending || manualRefreshing}
                  error={listError}
                  pendingAction={pendingAction}
                  onCreate={openCreate}
                  onRetry={() => void handleRefresh()}
                  onDetails={openDetails}
                  onAction={(kind, sandbox) => void handleAction(kind, sandbox)}
                  onExec={openTerminal}
                />
              </section>
            </>
          )}
        </main>
      </div>

      {createOpen ? (
        <CreateVmModal
          open
          initialImage={createImage ?? undefined}
          onClose={() => {
            setCreateOpen(false);
            setCreateImage(null);
          }}
          onGoToImages={() => {
            setCreateOpen(false);
            setCreateImage(null);
            setOpenPullOnImages(true);
            navigate("images");
          }}
        />
      ) : null}

      <Modal
        open={deleteOpen}
        onClose={closeDelete}
        size="sm"
        title="Delete this sandbox?"
        footer={
          <>
            <Button onClick={closeDelete} disabled={deleting}>
              Cancel
            </Button>
            <Button variant="danger" loading={deleting} onClick={() => void confirmDelete()}>
              Delete sandbox
            </Button>
          </>
        }
      >
        <p className="text-sm leading-relaxed text-gray-600 dark:text-gray-300">
          <strong className="font-semibold text-gray-900 dark:text-white">
            {deleteTarget?.name ?? "This sandbox"}
          </strong>{" "}
          and its writable volume will be removed permanently. Any running workload stops
          immediately and the scratch disk is discarded. This cannot be undone.
        </p>
      </Modal>

      {detailSnapshot !== null ? (
        <VmDetailDrawer
          sandbox={detailSandbox}
          open={drawerOpen}
          images={images}
          pendingAction={pendingAction}
          onClose={closeDrawer}
          onAction={(kind, sandbox) => void handleAction(kind, sandbox)}
          onExec={openTerminal}
        />
      ) : null}

      {terminalTarget !== null ? (
        <TerminalDialog sandbox={terminalSandbox} open={terminalOpen} onClose={closeTerminal} />
      ) : null}
    </div>
  );
}
