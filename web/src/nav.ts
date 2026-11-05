import {
  FileCode2,
  Inbox,
  ScrollText,
  Send,
  Settings,
  ShieldCheck,
  Telescope,
  Workflow,
  type LucideIcon,
} from "lucide-react";

export interface NavItem {
  path: string;
  label: string;
  icon: LucideIcon;
  emptyTitle: string;
  emptyBody: string;
  /** Shown to admins only (006-AC5). */
  admin?: boolean;
}

export const NAV: NavItem[] = [
  {
    path: "/inbox",
    label: "Inbox",
    icon: Inbox,
    emptyTitle: "Nothing to read yet",
    emptyBody: "Changes your watches pick up land here, newest first.",
  },
  {
    path: "/watches",
    label: "Watches",
    icon: Telescope,
    emptyTitle: "No watches yet",
    emptyBody:
      "A watch follows one thing upstream: a repository's releases, a pull or merge request, or an image's tags.",
  },
  {
    path: "/channels",
    label: "Channels",
    icon: Send,
    emptyTitle: "No channels yet",
    emptyBody:
      "Channels are where matching changes go: Telegram, Nextcloud Talk, email, a webhook or a script.",
  },
  {
    path: "/rules",
    label: "Rules",
    icon: Workflow,
    emptyTitle: "No rules yet",
    emptyBody: "A rule sends the changes of a watch that pass its filter to one or more channels.",
  },
  {
    path: "/scripts",
    label: "Scripts",
    icon: FileCode2,
    emptyTitle: "No scripts yet",
    emptyBody:
      "Scripts run on a matching change and can call other services with what they receive.",
  },
  {
    path: "/audit",
    label: "Audit",
    icon: ScrollText,
    emptyTitle: "No activity recorded",
    emptyBody:
      "Every change to watches, channels, rules and scripts is listed here with who made it.",
  },
  {
    path: "/settings",
    label: "Settings",
    icon: Settings,
    emptyTitle: "Nothing to configure yet",
    emptyBody: "Credentials and account preferences.",
  },
  {
    path: "/admin/users",
    label: "Administration",
    icon: ShieldCheck,
    admin: true,
    emptyTitle: "No accounts yet",
    emptyBody: "Accounts appear here after their first sign-in.",
  },
];
