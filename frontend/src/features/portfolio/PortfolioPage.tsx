/**
 * PortfolioPage — `/portfolio` (Task 10.3.5 surface).
 */
import { RequireAuth } from '@/features/auth/guards';

import { MarginViewPanel } from './MarginViewPanel';
import { Portfolio } from './Portfolio';

export default function PortfolioPage() {
  return (
    <div className="mx-auto max-w-5xl p-4">
      <h1 className="mb-4 text-2xl font-semibold">Portfolio</h1>
      <RequireAuth>
        <Portfolio />
        <MarginViewPanel />
      </RequireAuth>
    </div>
  );
}
