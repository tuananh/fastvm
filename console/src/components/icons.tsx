import {
  ArrowPathIcon as ArrowPath,
  ArrowRightStartOnRectangleIcon as ArrowRightStartOnRectangle,
  ChartBarIcon as ChartBar,
  CheckIcon as Check,
  ChevronDownIcon as ChevronDown,
  CircleStackIcon as CircleStack,
  ClockIcon as Clock,
  CommandLineIcon as CommandLine,
  ComputerDesktopIcon as ComputerDesktop,
  CpuChipIcon as CpuChip,
  DocumentDuplicateIcon as DocumentDuplicate,
  EllipsisHorizontalIcon as EllipsisHorizontal,
  GlobeAltIcon as GlobeAlt,
  InformationCircleIcon as InformationCircle,
  ListBulletIcon as ListBullet,
  MoonIcon as Moon,
  PauseIcon as Pause,
  PlusIcon as Plus,
  ServerIcon as Server,
  ServerStackIcon as ServerStack,
  ShieldCheckIcon as ShieldCheck,
  Square3Stack3DIcon as Square3Stack3D,
  SunIcon as Sun,
  SwatchIcon as Swatch,
  TrashIcon as Trash,
  UserIcon as User,
  XMarkIcon as XMark,
} from "@heroicons/react/24/outline";
import { BoltIcon as BoltSolid, PlayIcon as PlaySolid } from "@heroicons/react/24/solid";
import type { ComponentType, SVGProps } from "react";

export type IconProps = SVGProps<SVGSVGElement>;

type HeroIcon = ComponentType<SVGProps<SVGSVGElement>>;

/** Wraps a Heroicon so it inherits the surrounding font size by default. */
function outline(Component: HeroIcon) {
  return function Icon({ width = "1em", height = "1em", strokeWidth = 1.8, ...props }: IconProps) {
    return (
      <Component
        width={width}
        height={height}
        strokeWidth={strokeWidth}
        aria-hidden="true"
        focusable="false"
        {...props}
      />
    );
  };
}

/** Filled Heroicon (for small brand/action glyphs). */
function solid(Component: HeroIcon) {
  return function Icon({ width = "1em", height = "1em", ...props }: IconProps) {
    return (
      <Component width={width} height={height} aria-hidden="true" focusable="false" {...props} />
    );
  };
}

export const PlusIcon = outline(Plus);
export const RefreshIcon = outline(ArrowPath);
export const ChevronDownIcon = outline(ChevronDown);
export const UserIcon = outline(User);
export const LogoutIcon = outline(ArrowRightStartOnRectangle);
export const PauseIcon = outline(Pause);
export const MoonIcon = outline(Moon);
export const PlayIcon = solid(PlaySolid);
export const TrashIcon = outline(Trash);
export const InfoIcon = outline(InformationCircle);
export const ServerIcon = outline(Server);
export const HardDriveIcon = outline(CircleStack);
export const CheckIcon = outline(Check);
export const MonitorIcon = outline(ComputerDesktop);
export const SunIcon = outline(Sun);
export const ActivityIcon = outline(ChartBar);
export const ClockIcon = outline(Clock);
export const TerminalIcon = outline(CommandLine);
export const ListBulletIcon = outline(ListBullet);
export const CpuIcon = outline(CpuChip);
export const MemoryIcon = outline(ServerStack);
export const NetworkIcon = outline(GlobeAlt);
export const ShieldCheckIcon = outline(ShieldCheck);
export const GaugeIcon = outline(ChartBar);
export const LayersIcon = outline(Square3Stack3D);
export const ListIcon = outline(ListBullet);
export const MoreIcon = outline(EllipsisHorizontal);
export const ThemeModeIcon = outline(Swatch);
export const ZapIcon = solid(BoltSolid);
export const BoltIcon = solid(BoltSolid);
export const CopyIcon = outline(DocumentDuplicate);
export const XMarkIcon = outline(XMark);
