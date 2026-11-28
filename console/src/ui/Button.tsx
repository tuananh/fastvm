import { ArrowPathIcon } from "@heroicons/react/24/outline";
import type { ButtonHTMLAttributes, ReactNode } from "react";
import { cn } from "./cn";

type Variant = "primary" | "secondary" | "ghost" | "danger";
type Size = "xs" | "sm" | "md";

const base =
  "inline-flex items-center justify-center gap-2 rounded-lg text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-4 focus-visible:ring-indigo-200 disabled:pointer-events-none disabled:opacity-50 dark:focus-visible:ring-indigo-900";

const variants: Record<Variant, string> = {
  primary:
    "border border-[var(--app-accent)] bg-[var(--app-accent)] text-[var(--om-on-accent)] hover:opacity-90",
  secondary:
    "border border-gray-300 bg-white text-gray-700 shadow-sm hover:bg-gray-50 hover:text-gray-900 dark:border-gray-700 dark:bg-gray-900 dark:text-gray-200 dark:hover:bg-gray-800 dark:hover:text-white",
  ghost:
    "text-gray-600 hover:bg-gray-100 hover:text-gray-900 dark:text-gray-300 dark:hover:bg-gray-800 dark:hover:text-white",
  danger:
    "border border-red-200 bg-red-50 text-red-700 hover:bg-red-100 dark:border-red-900 dark:bg-red-950 dark:text-red-300 dark:hover:bg-red-900/60",
};

const sizes: Record<Size, string> = {
  xs: "h-8 px-3 text-xs",
  sm: "h-9 px-3.5",
  md: "h-10 px-4",
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
  loading?: boolean;
  icon?: ReactNode;
}

/** HyperUI-style button: rounded-lg, subtle borders, indigo primary. */
export function Button({
  variant = "secondary",
  size = "sm",
  loading = false,
  icon,
  className,
  children,
  disabled,
  type = "button",
  ...rest
}: ButtonProps) {
  return (
    <button
      type={type}
      className={cn(base, variants[variant], sizes[size], className)}
      disabled={disabled === true || loading}
      {...rest}
    >
      {loading ? <Spinner className="size-4" /> : icon}
      {children}
    </button>
  );
}

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  label: string;
  variant?: "ghost" | "secondary";
}

/** Square icon-only button with an accessible label. */
export function IconButton({
  label,
  variant = "ghost",
  className,
  children,
  ...rest
}: IconButtonProps) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      className={cn(
        "inline-flex size-9 items-center justify-center rounded-lg transition-colors focus-visible:outline-none focus-visible:ring-4 focus-visible:ring-indigo-200 disabled:opacity-50 dark:focus-visible:ring-indigo-900",
        variant === "ghost"
          ? "text-gray-500 hover:bg-gray-100 hover:text-gray-900 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white"
          : "border border-gray-300 bg-white text-gray-700 shadow-sm hover:bg-gray-50 dark:border-gray-700 dark:bg-gray-900 dark:text-gray-200 dark:hover:bg-gray-800",
        className,
      )}
      {...rest}
    >
      {children}
    </button>
  );
}

export function Spinner({ className = "size-4" }: { className?: string }) {
  return <ArrowPathIcon className={cn("animate-spin", className)} aria-hidden="true" />;
}
