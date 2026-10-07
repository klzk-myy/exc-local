/**
 * Reports & transparency (Task 10.3.28) — download center, fee schedule,
 * solvency/PoR, system info, announcements.
 */
import { useState } from 'react';

import { DownloadCenter } from './DownloadCenter';
import { ExportJobsPanel } from './ExportJobsPanel';
import { AnnouncementsPanel, FeeSchedulePanel, SolvencyPanel, SystemInfoPanel } from './panels';
import { TcaPanel } from './TcaPanel';

const TABS = [
  { id: 'downloads', label: 'Downloads' },
  { id: 'tca', label: 'TCA' },
  { id: 'fees', label: 'Fees' },
  { id: 'solvency', label: 'Solvency' },
  { id: 'system', label: 'System' },
  { id: 'announcements', label: 'Announcements' },
] as const;
type TabId = (typeof TABS)[number]['id'];

export default function ReportsPage() {
  const [tab, setTab] = useState<TabId>('downloads');
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Reports &amp; transparency</h1>
        <p className="text-sm text-neutral-500">
          Statements, tax reports, confirmations, fee schedule, proof-of-reserves and system status.
        </p>
      </header>
      <div
        role="tablist"
        aria-label="Report sections"
        className="flex gap-1 relative overflow-x-auto border-b border-neutral-800"
        tabIndex={0}
      >
        {TABS.map((t) => (
          <button
            key={t.id}
            role="tab"
            aria-selected={tab === t.id}
            className={`whitespace-nowrap px-3 py-2 text-sm ${
              tab === t.id
                ? 'border-b-2 border-sky-500 text-neutral-100'
                : 'text-neutral-400 hover:text-neutral-200'
            }`}
            onClick={() => {
              setTab(t.id);
            }}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'downloads' ? (
        <div className="space-y-4">
          <DownloadCenter />
          <ExportJobsPanel />
        </div>
      ) : null}
      {tab === 'tca' ? <TcaPanel /> : null}
      {tab === 'fees' ? <FeeSchedulePanel /> : null}
      {tab === 'solvency' ? <SolvencyPanel /> : null}
      {tab === 'system' ? <SystemInfoPanel /> : null}
      {tab === 'announcements' ? <AnnouncementsPanel /> : null}
    </div>
  );
}
