import { useMemo, useState, useEffect, useRef } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import AlphabetRail from '../components/AlphabetRail';
import FilterBar from '../components/FilterBar';
import ProgressBar from '../components/ProgressBar';
import { formatRelativeDate, providerArtistUrl, PROVIDER_LABEL } from '../lib/format';
import { usePlayer } from '../components/Player';
import { useToast } from '../components/Toast';
import type { Artist } from '../types/index';

type LibrarySort = 'az' | 'recent';

export default function Library() {
  const { toast } = useToast();
  const queryClient = useQueryClient();
  const player = usePlayer();
  // Lift fixed bottom bars above the player when it's active: player top is
  // ~3.5rem above the mobile nav (3.5rem) — on desktop it sits at bottom-0.
  const floatClass = player.current
    ? 'bottom-[calc(7.5rem+env(safe-area-inset-bottom,0px))] md:bottom-[4.5rem]'
    : 'bottom-4';
  const [filter, setFilter] = useState('');
  const [debouncedFilter, setDebouncedFilter] = useState('');
  const [selecting, setSelecting] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  // IDs queued by the bulk-refresh action — the sync-status poll is scoped to
  // these so stale entries for other artists don't confuse the progress bar.
  const [refreshIds, setRefreshIds] = useState<number[]>([]);
  const [expandSync, setExpandSync] = useState(false);
  const seenSync = useRef(false);
  const syncTicks = useRef(0);
  const [sort, setSort] = useState<LibrarySort>(
    () => (sessionStorage.getItem('library-sort') as LibrarySort) || 'az',
  );

  const { data: artists, isLoading } = useQuery({
    queryKey: ['artists'],
    queryFn: api.listArtists,
  });

  const { data: searchResults } = useQuery({
    queryKey: ['library-search', debouncedFilter],
    queryFn: () => api.searchLibrary(debouncedFilter),
    enabled: debouncedFilter.length >= 2,
  });

  const matchedArtistIds = useMemo(() => {
    if (!debouncedFilter || !searchResults) return null;
    const ids = new Set<number>();
    for (const r of searchResults) ids.add(r.artist_id);
    return ids;
  }, [debouncedFilter, searchResults]);

  const filteredArtists = useMemo(() => {
    if (!artists) return [];
    if (!debouncedFilter) return artists;
    const q = debouncedFilter.toLowerCase();
    return artists.filter((a) => {
      if (a.name.toLowerCase().includes(q)) return true;
      return matchedArtistIds?.has(a.id) ?? false;
    });
  }, [artists, debouncedFilter, matchedArtistIds]);

  const matchCountByArtist = useMemo(() => {
    if (!searchResults || !debouncedFilter) return null;
    const counts = new Map<number, number>();
    for (const r of searchResults) {
      counts.set(r.artist_id, (counts.get(r.artist_id) || 0) + 1);
    }
    return counts;
  }, [searchResults, debouncedFilter]);

  const grouped = useMemo(() => groupByLetter(filteredArtists), [filteredArtists]);
  const activeLetters = useMemo(() => new Set(grouped.map((g) => g.letter)), [grouped]);
  const recentArtists = useMemo(
    () => [...filteredArtists].sort((a, b) => b.created_at.localeCompare(a.created_at) || a.name.localeCompare(b.name)),
    [filteredArtists],
  );
  // Display order for shift-click range selection — grouped re-sorts for A–Z,
  // so the id sequence has to come from the rendered list, not filteredArtists.
  const displayOrder = useMemo(
    () => (sort === 'recent' ? recentArtists : grouped.flatMap((g) => g.artists)).map((a) => a.id),
    [sort, recentArtists, grouped],
  );
  const setSortPersist = (s: LibrarySort) => {
    setSort(s);
    sessionStorage.setItem('library-sort', s);
  };

  const bulkWatch = useMutation({
    mutationFn: ({ ids, enabled }: { ids: number[]; enabled: boolean }) =>
      api.setArtistsNewReleases(ids, enabled),
    onSuccess: (res, vars) => {
      queryClient.invalidateQueries({ queryKey: ['artists'] });
      toast(
        vars.enabled
          ? `Watching ${res.updated} artist(s) for new releases`
          : `Stopped watching ${res.updated} artist(s)`,
        'success',
      );
      setSelected(new Set());
      setSelecting(false);
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const bulkRefresh = useMutation({
    mutationFn: (ids: number[]) => api.refreshArtists(ids),
    onSuccess: (res) => {
      seenSync.current = false;
      syncTicks.current = 0;
      setRefreshIds(res.queued > 0 ? res.ids : []);
      toast(`Refreshing ${res.queued} artist(s)`, 'success');
      setSelected(new Set());
      setSelecting(false);
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  // Poll live sync progress for the artists we just queued; stops when every
  // one reports inactive (or on a ~5min backstop). Always enabled at a slow
  // ambient rate so syncs started on another device show here too — sync
  // state is shared server-side.
  const { data: syncStatus } = useQuery({
    queryKey: ['artist-sync'],
    queryFn: api.getSyncStatus,
    refetchInterval: (query) =>
      (query.state.data?.items ?? []).some((i) => i.active) ? 2000 : 20_000,
  });
  const refreshSync = useMemo(() => {
    if (refreshIds.length === 0) return syncStatus?.items ?? [];
    const ids = new Set(refreshIds);
    return (syncStatus?.items ?? []).filter((i) => ids.has(i.artist_id));
  }, [syncStatus, refreshIds]);
  const ambientSyncs = refreshSync.filter((i) => i.active);

  useEffect(() => {
    if (refreshIds.length === 0 || !syncStatus) return;
    syncTicks.current += 1;
    if (refreshSync.length > 0) seenSync.current = true;
    const allDone = seenSync.current && refreshSync.every((i) => !i.active);
    if (allDone || syncTicks.current > 150) {
      setRefreshIds([]);
      queryClient.invalidateQueries({ queryKey: ['artists'] });
      if (allDone) toast('Discographies refreshed', 'success');
    }
  }, [syncStatus, refreshIds.length, refreshSync, queryClient, toast]);

  const toggleSelect = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // Anchor for shift+click range selection — the last plain-clicked artist in
  // display order. Shift+click adds every artist between anchor and target.
  const lastClicked = useRef<number | null>(null);
  useEffect(() => {
    if (!selecting) lastClicked.current = null;
  }, [selecting]);

  const handleSelect = (id: number, shift: boolean) => {
    if (shift && lastClicked.current != null) {
      const from = displayOrder.indexOf(lastClicked.current);
      const to = displayOrder.indexOf(id);
      if (from >= 0 && to >= 0) {
        const [lo, hi] = from < to ? [from, to] : [to, from];
        setSelected((prev) => {
          const next = new Set(prev);
          for (const artistId of displayOrder.slice(lo, hi + 1)) next.add(artistId);
          return next;
        });
        return; // anchor stays on the last plain click
      }
    }
    toggleSelect(id);
    lastClicked.current = id;
  };

  const rowProps = (artist: Artist) => ({
    selecting,
    selected: selected.has(artist.id),
    onSelect: (e: { shiftKey: boolean }) => handleSelect(artist.id, e.shiftKey),
  });

  const scrollRestored = useRef(false);

  useEffect(() => {
    if (!artists?.length || scrollRestored.current) return;
    const saved = sessionStorage.getItem('library-scroll');
    if (saved) {
      requestAnimationFrame(() => window.scrollTo(0, parseInt(saved, 10)));
    }
    scrollRestored.current = true;
  }, [artists]);

  useEffect(() => {
    const onScroll = () => sessionStorage.setItem('library-scroll', String(window.scrollY));
    window.addEventListener('scroll', onScroll, { passive: true });
    return () => window.removeEventListener('scroll', onScroll);
  }, []);

  if (isLoading) {
    return (
      <div>
        <h2 className="text-lg font-bold mb-3">Watchlist</h2>
        <div className="space-y-1">
          {[...Array(4)].map((_, i) => (
            <div key={i} className="flex items-center gap-3 bg-zinc-800/40 rounded-lg p-2.5 animate-pulse">
              <div className="w-11 h-11 rounded-full bg-zinc-700" />
              <div className="flex-1 space-y-2">
                <div className="h-3.5 bg-zinc-700 rounded w-28" />
                <div className="h-2.5 bg-zinc-800 rounded w-16" />
              </div>
            </div>
          ))}
        </div>
      </div>
    );
  }

  if (!artists?.length) {
    return (
      <div className="text-center py-16">
        <svg className="w-12 h-12 mx-auto text-zinc-700 mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
          <path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" />
        </svg>
        <p className="text-zinc-500">Your crate is empty</p>
        <p className="text-zinc-600 text-sm mt-1.5">Search for artists to start watching music</p>
        <Link
          to="/search"
          className="inline-block mt-5 px-5 py-2.5 bg-white text-zinc-900 rounded-lg font-medium text-sm active:scale-[0.97] transition-transform"
        >
          Search Music
        </Link>
      </div>
    );
  }

  return (
    <div className="relative">
      <div className="sticky top-0 z-30 -mx-4 bg-zinc-950 px-4 pt-1 pb-2">
      <h2 className="text-lg font-bold mb-3">Watchlist</h2>

      <div className="flex items-start gap-2">
        <div className="flex-1 min-w-0">
          <FilterBar
            value={filter}
            onChange={(v) => {
              setFilter(v);
              setDebouncedFilter(v);
            }}
            debounceMs={300}
            placeholder="Filter by artist or song title..."
          />
        </div>
        <button
          onClick={() => {
            setSelecting((v) => !v);
            setSelected(new Set());
          }}
          className={`px-2.5 py-2 rounded-lg text-xs font-medium shrink-0 transition-colors ${
            selecting ? 'bg-zinc-700 text-zinc-100' : 'bg-zinc-800/60 text-zinc-500 hover:text-zinc-300'
          }`}
        >
          Select
        </button>
        <div className="flex rounded-lg bg-zinc-800/60 p-0.5 shrink-0">
          {(['az', 'recent'] as const).map((s) => (
            <button
              key={s}
              onClick={() => setSortPersist(s)}
              className={`px-2.5 py-1.5 rounded-md text-xs font-medium transition-colors ${
                sort === s ? 'bg-zinc-700 text-zinc-100' : 'text-zinc-500 hover:text-zinc-300'
              }`}
            >
              {s === 'az' ? 'A–Z' : 'Recent'}
            </button>
          ))}
        </div>
      </div>
      </div>

      {sort === 'az' && <AlphabetRail activeLetters={activeLetters} />}

      <div className={sort === 'az' ? 'pr-5' : ''}>
        {debouncedFilter && filteredArtists.length === 0 && (
          <p className="text-zinc-500 text-sm text-center py-6">No matches</p>
        )}
        {sort === 'recent' && (
          <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6 gap-3">
            {recentArtists.map((artist) => (
              <ArtistCard
                key={artist.id}
                artist={artist}
                addedLabel={formatRelativeDate(artist.created_at)}
                {...rowProps(artist)}
              />
            ))}
          </div>
        )}
        {sort === 'az' && grouped.map(({ letter, artists: group }) => (
          <div key={letter} id={`section-${letter}`}>
            {grouped.length > 1 && (
              <p className="text-[11px] font-semibold text-zinc-500 uppercase tracking-wider mt-3 mb-1 first:mt-0 scroll-mt-24">
                {letter}
              </p>
            )}
            <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6 gap-3">
              {group.map((artist) => (
                <ArtistCard
                  key={artist.id}
                  artist={artist}
                  matchCount={matchCountByArtist?.get(artist.id)}
                  isTrackMatch={!!matchedArtistIds?.has(artist.id) && !artist.name.toLowerCase().includes(debouncedFilter.toLowerCase())}
                  {...rowProps(artist)}
                />
              ))}
            </div>
          </div>
        ))}
      </div>

      {selecting && (
        <div className={`fixed ${floatClass} left-1/2 -translate-x-1/2 z-40 flex items-center gap-2 bg-zinc-800 border border-zinc-700 rounded-xl px-3 py-2 shadow-xl shadow-black/40`}>
          <button
            onClick={() => setSelected(new Set(filteredArtists.map((a) => a.id)))}
            className="text-xs text-zinc-400 hover:text-zinc-200 px-1"
          >
            All
          </button>
          <span className="text-xs text-zinc-500 whitespace-nowrap">{selected.size} selected</span>
          <button
            onClick={() => bulkWatch.mutate({ ids: [...selected], enabled: true })}
            disabled={selected.size === 0 || bulkWatch.isPending}
            className="px-2.5 py-1.5 rounded-lg bg-emerald-700 hover:bg-emerald-600 text-xs font-medium disabled:opacity-40 whitespace-nowrap"
          >
            Watch
          </button>
          <button
            onClick={() => bulkWatch.mutate({ ids: [...selected], enabled: false })}
            disabled={selected.size === 0 || bulkWatch.isPending}
            className="px-2.5 py-1.5 rounded-lg bg-zinc-700 hover:bg-zinc-600 text-xs font-medium disabled:opacity-40 whitespace-nowrap"
          >
            Unwatch
          </button>
          <button
            onClick={() => bulkRefresh.mutate(
              filteredArtists.filter((a) => selected.has(a.id) && a.provider !== 'local').map((a) => a.id),
            )}
            disabled={!filteredArtists.some((a) => selected.has(a.id) && a.provider !== 'local') || bulkRefresh.isPending}
            title="Refresh discographies (provider-linked artists only)"
            className="px-2.5 py-1.5 rounded-lg bg-blue-700 hover:bg-blue-600 text-xs font-medium disabled:opacity-40 whitespace-nowrap"
          >
            Refresh
          </button>
          <button
            onClick={() => {
              setSelecting(false);
              setSelected(new Set());
            }}
            className="px-2 py-1.5 text-xs text-zinc-500 hover:text-zinc-300"
          >
            Cancel
          </button>
        </div>
      )}

      {(refreshIds.length > 0 || ambientSyncs.length > 0) && (
        <div className={`fixed ${floatClass} left-1/2 -translate-x-1/2 z-40 w-[calc(100%-2rem)] max-w-sm`}>
          <button
            onClick={() => setExpandSync((v) => !v)}
            className="w-full flex items-center gap-2.5 bg-zinc-800 border border-zinc-700 rounded-xl px-3 py-2 shadow-xl shadow-black/40 text-left"
          >
            <div className="w-3.5 h-3.5 border-2 border-blue-500/40 border-t-blue-400 rounded-full animate-spin shrink-0" />
            <div className="min-w-0 flex-1">
              <p className="text-xs text-zinc-300 whitespace-nowrap">
                {refreshIds.length > 0
                  ? `Refreshing discographies — ${refreshSync.filter((i) => !i.active).length} of ${refreshIds.length} artists`
                  : `Syncing discography — ${ambientSyncs.length} artist(s)`}
              </p>
              {!expandSync && (() => {
                const active = refreshSync.find((i) => i.active);
                if (!active) return null;
                const name = filteredArtists.find((a) => a.id === active.artist_id)?.name ?? 'artist';
                return (
                  <p className="text-[10px] text-zinc-500 truncate">
                    {name}{active.total > 0 ? ` — ${active.done}/${active.total}` : ''}{active.current ? `: ${active.current}` : ''}
                  </p>
                );
              })()}
            </div>
            <svg
              className={`w-3.5 h-3.5 text-zinc-500 shrink-0 transition-transform ${expandSync ? '' : 'rotate-180'}`}
              viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"
            >
              <path d="m6 9 6 6 6-6" />
            </svg>
          </button>

          {expandSync && (
            <div className="mt-1.5 bg-zinc-800 border border-zinc-700 rounded-xl shadow-xl shadow-black/40 max-h-56 overflow-y-auto">
              {(() => {
                const items = syncStatus?.items ?? [];
                const nameFor = (id: number) =>
                  artists?.find((a) => a.id === id)?.name ?? `Artist #${id}`;
                // refreshIds are the queued set; ambient actives not in it
                // (started on another device) get appended at the end.
                const ids = refreshIds.length > 0
                  ? [...refreshIds, ...ambientSyncs.map((i) => i.artist_id).filter((id) => !refreshIds.includes(id))]
                  : ambientSyncs.map((i) => i.artist_id);
                return ids.map((artistId) => {
                  const item = items.find((i) => i.artist_id === artistId);
                  const queued = item?.active && item.phase === 'queued';
                  const syncing = item?.active && !queued;
                  return (
                    <div key={artistId} className="flex items-center gap-2.5 px-3 py-2 border-b border-zinc-700/60 last:border-0">
                      {queued ? (
                        <svg className="w-3.5 h-3.5 text-zinc-500 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="10" /><path d="M12 6v6l4 2" /></svg>
                      ) : syncing ? (
                        <div className="w-3.5 h-3.5 border-2 border-blue-500/40 border-t-blue-400 rounded-full animate-spin shrink-0" />
                      ) : (
                        <svg className="w-3.5 h-3.5 text-emerald-400 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6 9 17l-5-5" /></svg>
                      )}
                      <div className="min-w-0 flex-1">
                        <p className="text-xs text-zinc-200 truncate">{nameFor(artistId)}</p>
                        {queued && <p className="text-[10px] text-zinc-500">waiting for another sync…</p>}
                        {syncing && (
                          <p className="text-[10px] text-zinc-500 truncate">
                            {item!.total > 0 ? `${item!.done}/${item!.total}` : 'contacting provider…'}
                            {item!.current ? ` — ${item!.current}` : ''}
                          </p>
                        )}
                        {!item?.active && <p className="text-[10px] text-zinc-600">done</p>}
                      </div>
                    </div>
                  );
                });
              })()}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function ArtistCard({
  artist,
  matchCount,
  isTrackMatch,
  addedLabel,
  selecting,
  selected,
  onSelect,
}: {
  artist: Artist;
  matchCount?: number;
  isTrackMatch?: boolean;
  addedLabel?: string;
  selecting?: boolean;
  selected?: boolean;
  onSelect?: (e: { shiftKey: boolean }) => void;
}) {
  const owned = artist.owned_tracks ?? 0;
  const total = artist.total_tracks ?? 0;

  const inner = (
    <div
      className={`rounded-lg overflow-hidden transition-colors ${
        selected ? 'ring-2 ring-emerald-500 bg-emerald-900/20' : 'bg-zinc-800/40 hover:bg-zinc-800/70'
      }`}
    >
      <div className="relative aspect-square bg-zinc-700">
        {artist.image_url ? (
          <img src={artist.image_url} alt={artist.name} className="w-full h-full object-cover" loading="lazy" />
        ) : (
          <div className="w-full h-full flex items-center justify-center text-zinc-500">
            <svg className="w-10 h-10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" /></svg>
          </div>
        )}
        {selecting && (
          <div
            className={`absolute top-1.5 right-1.5 w-5 h-5 rounded-full border-2 flex items-center justify-center ${
              selected ? 'bg-emerald-500 border-emerald-500' : 'bg-zinc-900/60 border-zinc-400'
            }`}
          >
            {selected && (
              <svg className="w-3 h-3 text-zinc-900" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round" strokeLinejoin="round">
                <path d="M20 6 9 17l-5-5" />
              </svg>
            )}
          </div>
        )}
        {(artist.provider === 'local' || artist.orphaned) && (
          <span
            className={`absolute top-1.5 left-1.5 px-1.5 py-0.5 rounded text-[9px] font-medium uppercase ${
              artist.orphaned ? 'bg-red-900/80 text-red-300' : 'bg-amber-900/80 text-amber-300'
            }`}
          >
            {artist.orphaned ? 'orphaned' : 'not linked'}
          </span>
        )}
        {!selecting && providerArtistUrl(artist.provider, artist.provider_id) && (
          <button
            onClick={(e) => {
              e.preventDefault();
              e.stopPropagation();
              window.open(providerArtistUrl(artist.provider, artist.provider_id), '_blank', 'noopener');
            }}
            className="absolute bottom-1.5 right-1.5 p-1 rounded bg-zinc-900/70 text-zinc-400 hover:text-zinc-200 transition-colors"
            title={`View on ${PROVIDER_LABEL[artist.provider] ?? artist.provider}`}
          >
            <svg className="w-3 h-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" /><polyline points="15 3 21 3 21 9" /><line x1="10" y1="14" x2="21" y2="3" />
            </svg>
          </button>
        )}
      </div>
      <div className="p-2">
        <p className="text-xs font-medium truncate" title={artist.name}>{artist.name}</p>
        {isTrackMatch && matchCount ? (
          <p className="text-[10px] text-zinc-500 mt-0.5">{matchCount} matching track{matchCount > 1 ? 's' : ''}</p>
        ) : (
          <>
            {addedLabel && <p className="text-[10px] text-zinc-600">Added {addedLabel}</p>}
            <div className="mt-1">
              <ProgressBar owned={owned} total={total} />
            </div>
            <p className="text-[10px] text-zinc-500 mt-1 flex items-center justify-between">
              <span className="tabular-nums">{owned}/{total}</span>
              <span className={artist.status === 'owned' ? 'text-green-400' : ''}>
                {artist.status === 'owned' ? 'Owned' : artist.watch_new_releases ? 'Monitored' : 'Not monitored'}
              </span>
            </p>
          </>
        )}
      </div>
    </div>
  );

  if (selecting) {
    return (
      <button onClick={onSelect} className="text-left">
        {inner}
      </button>
    );
  }

  return <Link to={`/artist/${artist.id}`}>{inner}</Link>;
}

function groupByLetter(artists: Artist[]): { letter: string; artists: Artist[] }[] {
  const sorted = [...artists].sort((a, b) =>
    a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
  );

  const groups: { letter: string; artists: Artist[] }[] = [];
  for (const artist of sorted) {
    const first = artist.name[0]?.toUpperCase() ?? '#';
    const letter = /[A-Z]/.test(first) ? first : '#';
    const last = groups[groups.length - 1];
    if (last?.letter === letter) {
      last.artists.push(artist);
    } else {
      groups.push({ letter, artists: [artist] });
    }
  }
  return groups;
}
