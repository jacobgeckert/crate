import { useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Album } from '../types/index';
import { usePersistedQuery } from '../lib/persistedQuery';
import { useToast } from '../components/Toast';
import RefreshStatus, { Spinner } from '../components/RefreshStatus';

const TYPE_BADGES: Record<string, string> = {
  ep: 'EP',
  single: 'Single',
  compilation: 'Comp',
};

function formatReleaseDate(iso: string): string {
  const d = new Date(`${iso}T00:00:00Z`);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric', timeZone: 'UTC' });
}

function daysUntil(iso: string): string {
  const d = new Date(`${iso}T00:00:00Z`).getTime();
  if (isNaN(d)) return '';
  const now = new Date();
  const todayUtc = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
  const days = Math.round((d - todayUtc) / 86400000);
  if (days <= 0) return 'out today';
  if (days === 1) return 'tomorrow';
  if (days < 30) return `in ${days}d`;
  const months = Math.floor(days / 30);
  return months === 1 ? 'in ~1mo' : `in ~${months}mo`;
}

export default function Upcoming() {
  const queryClient = useQueryClient();
  const { data: releases, isLoading } = usePersistedQuery('upcoming-releases', ['upcoming-releases'], api.getUpcomingReleases);
  const { toast } = useToast();
  const [refreshing, setRefreshing] = useState(false);

  // The lookup is a quick local query; hold the indicator briefly so a click
  // visibly does something, then confirm with a toast.
  const refresh = async () => {
    setRefreshing(true);
    try {
      await Promise.all([
        queryClient.refetchQueries({ queryKey: ['upcoming-releases'] }, { throwOnError: true }),
        new Promise((r) => setTimeout(r, 600)),
      ]);
      const n = queryClient.getQueryData<Album[]>(['upcoming-releases'])?.length ?? 0;
      toast(n > 0 ? `Up to date · ${n} upcoming release${n === 1 ? '' : 's'}` : 'Up to date · no upcoming releases', 'success');
    } catch (err) {
      toast((err as Error).message, 'error');
    } finally {
      setRefreshing(false);
    }
  };

  // Group by month for section headers.
  const groups = useMemo(() => {
    const map = new Map<string, Album[]>();
    for (const a of releases ?? []) {
      const key = (a.release_date ?? '').slice(0, 7);
      const list = map.get(key) ?? [];
      list.push(a);
      map.set(key, list);
    }
    return [...map.entries()].map(([key, albums]) => {
      const d = new Date(`${key}-01T00:00:00Z`);
      return {
        label: isNaN(d.getTime())
          ? key
          : d.toLocaleDateString(undefined, { year: 'numeric', month: 'long', timeZone: 'UTC' }),
        albums,
      };
    });
  }, [releases]);

  return (
    <div className="p-4 max-w-2xl mx-auto">
      <div className="flex items-center justify-between mb-1">
        <h1 className="text-xl font-bold">Upcoming</h1>
        <button
          onClick={refresh}
          disabled={refreshing}
          className="text-xs text-zinc-400 bg-zinc-800 rounded-lg px-3 py-1.5 active:bg-zinc-700 transition-colors disabled:opacity-50"
        >
          {refreshing ? <span className="flex items-center gap-1.5"><Spinner />Refreshing…</span> : 'Refresh'}
        </button>
      </div>
      <p className="text-sm text-zinc-500 mb-4">Announced releases from your artists</p>

      {refreshing && <RefreshStatus label="Checking for announced releases" />}

      {isLoading && (
        <div className="space-y-2">
          {[...Array(4)].map((_, i) => (
            <div key={i} className="h-14 bg-zinc-800/40 rounded-lg animate-pulse" />
          ))}
        </div>
      )}

      {!isLoading && groups.length === 0 && (
        <div className="text-center py-12 text-zinc-500">
          <p className="text-sm">No upcoming releases.</p>
          <p className="text-xs mt-1 text-zinc-600">
            Announced releases appear here after a discography sync — refresh an artist or watch a new one.
          </p>
        </div>
      )}

      {groups.map((group) => (
        <div key={group.label} className={refreshing ? 'opacity-50 transition-opacity' : 'transition-opacity'}>
          <p className="text-[11px] font-semibold text-zinc-500 uppercase tracking-wider mt-4 mb-1.5 first:mt-0">
            {group.label}
          </p>
          <div className="space-y-1">
            {group.albums.map((album) => (
              <Link
                key={album.id}
                to={`/album/${album.id}`}
                className="flex items-center gap-2.5 bg-zinc-800/40 rounded-lg p-2 active:bg-zinc-800 transition-colors"
              >
                <div className="w-10 h-10 rounded bg-zinc-700 overflow-hidden shrink-0">
                  {album.cover_url ? (
                    <img src={album.cover_url} alt="" className="w-full h-full object-cover" onError={(e) => (e.target as HTMLImageElement).style.display = 'none'} />
                  ) : (
                    <div className="w-full h-full flex items-center justify-center text-zinc-500">
                      <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><rect x="3" y="3" width="18" height="18" rx="2" /><circle cx="12" cy="12" r="4" /><circle cx="12" cy="12" r="1" /></svg>
                    </div>
                  )}
                </div>
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-1.5">
                    <p className="font-medium text-sm truncate">{album.title}</p>
                    {TYPE_BADGES[album.record_type] && (
                      <span className="text-[9px] font-medium uppercase text-zinc-500 bg-zinc-800 px-1 py-px rounded shrink-0">
                        {TYPE_BADGES[album.record_type]}
                      </span>
                    )}
                  </div>
                  <p className="text-[11px] text-zinc-500 truncate">{album.artist_name}</p>
                </div>
                <div className="text-right shrink-0">
                  <p className="text-[11px] text-zinc-400">{album.release_date && formatReleaseDate(album.release_date)}</p>
                  <p className="text-[10px] text-green-400/80">{album.release_date && daysUntil(album.release_date)}</p>
                </div>
              </Link>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
