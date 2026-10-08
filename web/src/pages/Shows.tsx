import { useMemo } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Show } from '../types/index';

function formatDate(iso: string): string {
  const d = new Date(`${iso}T00:00:00`);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' });
}

function formatTime(t?: string): string {
  if (!t) return '';
  const [h, m] = t.split(':').map(Number);
  if (isNaN(h)) return '';
  const ampm = h >= 12 ? 'PM' : 'AM';
  return `${h % 12 || 12}:${String(m ?? 0).padStart(2, '0')} ${ampm}`;
}

function daysUntil(iso: string): string {
  const d = new Date(`${iso}T00:00:00`).getTime();
  if (isNaN(d)) return '';
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const days = Math.round((d - today) / 86400000);
  if (days <= 0) return 'today';
  if (days === 1) return 'tomorrow';
  if (days < 30) return `in ${days}d`;
  const months = Math.floor(days / 30);
  return months === 1 ? 'in ~1mo' : `in ~${months}mo`;
}

export default function Shows() {
  const queryClient = useQueryClient();
  const { data, isLoading, isFetching } = useQuery({
    queryKey: ['concerts'],
    queryFn: () => api.getConcerts(),
  });

  const refresh = () =>
    api.getConcerts(true).then((d) => queryClient.setQueryData(['concerts'], d));

  const groups = useMemo(() => {
    const map = new Map<string, Show[]>();
    for (const s of data?.shows ?? []) {
      const key = (s.date ?? '').slice(0, 7);
      const list = map.get(key) ?? [];
      list.push(s);
      map.set(key, list);
    }
    return [...map.entries()].map(([key, shows]) => {
      const d = new Date(`${key}-01T00:00:00`);
      return {
        label: isNaN(d.getTime())
          ? key
          : d.toLocaleDateString(undefined, { year: 'numeric', month: 'long' }),
        shows,
      };
    });
  }, [data]);

  return (
    <div className="max-w-2xl mx-auto">
      <div className="flex items-center justify-between mb-1">
        <h1 className="text-xl font-bold">Shows</h1>
        {data?.configured && (
          <button
            onClick={refresh}
            disabled={isFetching}
            className="text-xs text-zinc-400 bg-zinc-800 rounded-lg px-3 py-1.5 active:bg-zinc-700 transition-colors disabled:opacity-50"
          >
            {isFetching ? 'Refreshing…' : 'Refresh'}
          </button>
        )}
      </div>
      <p className="text-sm text-zinc-500 mb-4">Upcoming concerts for artists you watch</p>

      {isLoading && (
        <div className="space-y-2">
          {[...Array(4)].map((_, i) => (
            <div key={i} className="h-14 bg-zinc-800/40 rounded-lg animate-pulse" />
          ))}
        </div>
      )}

      {!isLoading && data && !data.configured && (
        <div className="text-center py-12 text-zinc-500">
          <p className="text-sm">Concert tracking isn't set up yet.</p>
          <p className="text-xs mt-1 text-zinc-600">
            Add a Ticketmaster API key and postal code in{' '}
            <Link to="/settings" className="text-zinc-300 underline">Settings → Concerts</Link>.
          </p>
        </div>
      )}

      {!isLoading && data?.configured && groups.length === 0 && (
        <div className="text-center py-12 text-zinc-500">
          <p className="text-sm">No upcoming shows found for your artists.</p>
          <p className="text-xs mt-1 text-zinc-600">
            Only Ticketmaster-listed events within your configured radius appear here.
          </p>
        </div>
      )}

      {groups.map((group) => (
        <div key={group.label}>
          <p className="text-[11px] font-semibold text-zinc-500 uppercase tracking-wider mt-4 mb-1.5 first:mt-0">
            {group.label}
          </p>
          <div className="space-y-1">
            {group.shows.map((show) => (
              <div key={`${show.event_id}-${show.artist_id}`} className="flex items-center gap-2.5 bg-zinc-800/40 rounded-lg p-2">
                <div className="w-10 h-10 rounded bg-zinc-700 overflow-hidden shrink-0">
                  {show.image ? (
                    <img src={show.image} alt="" className="w-full h-full object-cover" onError={(e) => (e.target as HTMLImageElement).style.display = 'none'} />
                  ) : (
                    <div className="w-full h-full flex items-center justify-center text-zinc-500">
                      <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M2 9a3 3 0 0 1 0 6v2a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-2a3 3 0 0 1 0-6V7a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2Z" /><path d="M13 5v2" /><path d="M13 11v2" /><path d="M13 17v2" /></svg>
                    </div>
                  )}
                </div>
                <div className="flex-1 min-w-0">
                  {show.artist_id ? (
                    <Link to={`/artist/${show.artist_id}`} className="font-medium text-sm truncate block hover:underline">
                      {show.artist_name}
                    </Link>
                  ) : (
                    <p className="font-medium text-sm truncate">{show.artist_name}</p>
                  )}
                  <p className="text-[11px] text-zinc-500 truncate">
                    {show.venue}{show.city ? ` · ${show.city}` : ''}{show.state ? `, ${show.state}` : ''}
                  </p>
                  {show.event_name !== show.artist_name && (
                    <p className="text-[10px] text-zinc-600 truncate">{show.event_name}</p>
                  )}
                </div>
                <div className="text-right shrink-0">
                  <p className="text-[11px] text-zinc-400">{formatDate(show.date)}{show.time ? ` · ${formatTime(show.time)}` : ''}</p>
                  <p className="text-[10px] text-green-400/80">{daysUntil(show.date)}</p>
                  {show.url && (
                    <a
                      href={show.url}
                      target="_blank"
                      rel="noreferrer"
                      className="text-[10px] text-zinc-300 underline"
                    >
                      Tickets
                    </a>
                  )}
                </div>
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
