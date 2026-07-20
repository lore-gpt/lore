import { Boxes, LayoutDashboard, type LucideIcon, Route } from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  label?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    items: [
      {
        id: "overview",
        title: "Overview",
        url: "/",
        icon: LayoutDashboard,
      },
      {
        id: "memories",
        title: "Memories",
        url: "/memories",
        icon: Boxes,
      },
      {
        id: "runs",
        title: "Runs",
        url: "/runs",
        icon: Route,
        badge: "soon",
        disabled: true,
      },
    ],
  },
];
