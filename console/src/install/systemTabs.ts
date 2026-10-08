// The tab bar of System, handed to the screen under it.
//
// Adapters, Policy, Backups and Updates are each a whole screen with its own
// heading and action, and each is also a tab of System (issue #154). Rather
// than teach all four that they might be a tab, Screen reads this: under
// System it shows System's heading and this bar, and keeps the screen's own
// action beside the heading.

import { createContext } from 'react';

import type { TabItem } from '@design';

export interface SystemTabBar {
  value: string;
  items: TabItem[];
  onChange: (value: string) => void;
}

export const SystemTabs = createContext<SystemTabBar | null>(null);
