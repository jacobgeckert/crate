export function Spinner({ className = 'w-3 h-3' }: { className?: string }) {
  return <span className={`inline-block border-2 border-zinc-500/40 border-t-zinc-200 rounded-full animate-spin ${className}`} />;
}

// Shown under a page header while Refresh runs. With a total it's a
// determinate bar; without one it pulses.
export default function RefreshStatus({ label, done, total }: { label: string; done?: number; total?: number }) {
  const pct = total ? Math.min(100, Math.round(((done ?? 0) / total) * 100)) : 0;
  return (
    <div className="mb-4" role="status">
      <div className="flex items-center gap-2 text-xs text-zinc-400 mb-1.5">
        <Spinner />
        <span>{label}…</span>
        {total ? <span className="ml-auto tabular-nums text-zinc-500">{done ?? 0}/{total}</span> : null}
      </div>
      <div className="h-1 bg-zinc-800 rounded-full overflow-hidden">
        {total ? (
          <div className="h-full bg-blue-500 rounded-full transition-all duration-300" style={{ width: `${pct}%` }} />
        ) : (
          <div className="h-full w-full bg-blue-500/60 rounded-full animate-pulse" />
        )}
      </div>
    </div>
  );
}
