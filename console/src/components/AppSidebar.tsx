import {
  ArrowTopRightOnSquareIcon,
  BookOpenIcon,
  CircleStackIcon,
  ServerStackIcon,
  Squares2X2Icon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import { cn } from "../ui";
import { BoltIcon } from "./icons";
import { ThemeMenu } from "./ThemeMenu";
import { UserMenu } from "./UserMenu";

export type ConsoleView = "sandboxes" | "images";

export interface AppSidebarProps {
  /** Whether the mobile slide-in sidebar is open (ignored on desktop). */
  open: boolean;
  /** Current top-level page. */
  view: ConsoleView;
  /** Switch pages; also closes the mobile drawer. */
  onNavigate: (view: ConsoleView) => void;
  /** Closes the mobile sidebar (overlay, close button and nav links call it). */
  onClose: () => void;
}

/** Link to the fastvm documentation in the repository. */
const DOCS_URL = "https://github.com/tuananh/fastvm/tree/main/docs";

const NAV_LINK =
  "flex items-center gap-3 rounded-lg px-4 py-2 text-sm font-medium text-gray-600 transition-colors hover:bg-gray-100 hover:text-gray-900 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white";
const NAV_LINK_ACTIVE =
  "flex items-center gap-3 rounded-lg bg-gray-100 px-4 py-2 text-sm font-medium text-gray-900 dark:bg-gray-800 dark:text-white";

/**
 * HyperUI-style dashboard sidebar: brand, section nav and account/theme footer.
 * Static on desktop, fixed slide-in drawer below `lg`. The page shell owns the
 * hamburger toggle; every link closes the drawer on mobile via `onClose`.
 */
export function AppSidebar({ open, view, onNavigate, onClose }: AppSidebarProps) {
  return (
    <>
      <div
        role="presentation"
        onClick={onClose}
        className={cn("fixed inset-0 z-30 bg-gray-900/50 lg:hidden", open ? "block" : "hidden")}
      />

      <aside
        id="app-sidebar"
        className={cn(
          "fixed inset-y-0 left-0 z-40 flex w-64 flex-col justify-between overflow-y-auto border-r border-gray-200 bg-white transition-transform duration-300 dark:border-gray-800 dark:bg-gray-900 lg:static lg:shrink-0 lg:translate-x-0",
          open ? "translate-x-0 max-lg:visible" : "-translate-x-full max-lg:invisible",
        )}
      >
        <div className="p-4">
          <div className="flex items-center justify-between gap-2">
            <div className="flex min-w-0 items-center gap-2">
              <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-indigo-600 text-white shadow-sm">
                <BoltIcon className="size-5" />
              </span>
              <span className="whitespace-nowrap text-sm font-semibold text-gray-900 dark:text-white">
                fastvm <span className="font-normal text-gray-500 dark:text-gray-400">console</span>
              </span>
            </div>

            <button
              type="button"
              onClick={onClose}
              aria-label="Close sidebar"
              className="rounded-md p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-900 focus-visible:ring-4 focus-visible:ring-indigo-200 focus-visible:outline-none lg:hidden dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white dark:focus-visible:ring-indigo-900"
            >
              <XMarkIcon className="size-5" />
            </button>
          </div>

          <nav aria-label="Dashboard" className="mt-6">
            <ul className="space-y-1">
              <li>
                <a
                  href="#overview"
                  onClick={(event) => {
                    event.preventDefault();
                    onNavigate("sandboxes");
                    onClose();
                  }}
                  aria-current={view === "sandboxes" ? "page" : undefined}
                  className={view === "sandboxes" ? NAV_LINK_ACTIVE : NAV_LINK}
                >
                  <Squares2X2Icon
                    className="size-5 shrink-0 text-indigo-600 dark:text-indigo-400"
                    aria-hidden="true"
                  />
                  Overview
                </a>
              </li>
              <li>
                <a
                  href="#sandboxes"
                  onClick={(event) => {
                    event.preventDefault();
                    onNavigate("sandboxes");
                    onClose();
                  }}
                  className={NAV_LINK}
                >
                  <ServerStackIcon
                    className="size-5 shrink-0 text-gray-500 dark:text-gray-400"
                    aria-hidden="true"
                  />
                  Sandboxes
                </a>
              </li>
              <li>
                <a
                  href="#images"
                  onClick={(event) => {
                    event.preventDefault();
                    onNavigate("images");
                    onClose();
                  }}
                  aria-current={view === "images" ? "page" : undefined}
                  className={view === "images" ? NAV_LINK_ACTIVE : NAV_LINK}
                >
                  <CircleStackIcon
                    className="size-5 shrink-0 text-gray-500 dark:text-gray-400"
                    aria-hidden="true"
                  />
                  Images
                </a>
              </li>
              <li>
                <a
                  href={DOCS_URL}
                  target="_blank"
                  rel="noreferrer"
                  onClick={onClose}
                  className={NAV_LINK}
                >
                  <BookOpenIcon
                    className="size-5 shrink-0 text-gray-500 dark:text-gray-400"
                    aria-hidden="true"
                  />
                  Documentation
                  <ArrowTopRightOnSquareIcon
                    className="ml-auto size-4 shrink-0 text-gray-400 dark:text-gray-500"
                    aria-hidden="true"
                  />
                </a>
              </li>
            </ul>
          </nav>
        </div>

        <div className="sticky inset-x-0 bottom-0 border-t border-gray-200 dark:border-gray-800">
          <div className="flex items-center gap-1 bg-white p-3 dark:bg-gray-900">
            <div className="min-w-0 flex-1">
              <UserMenu />
            </div>
            <ThemeMenu />
          </div>
        </div>
      </aside>
    </>
  );
}

export default AppSidebar;
