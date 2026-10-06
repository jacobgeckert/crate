import { useMemo, useState, useEffect, useRef } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import AlphabetRail from '../components/AlphabetRail';
import FilterBar from '../components/FilterBar';
import ProgressBar from '../components/ProgressBar';
import { formatRelativeDate } from '../lib/format';
import { useToast } from '../components/Toast';
import type { Artist } from '../types/index';

type LibrarySort = 'az' | 'recent';

const PROVIDER_ARTIST_URL: Record<string, (id: string) => string> = {
  musicbrainz: (id) => `https://musicbrainz.org/artist/${id}`,
  deezer: (id) => `https://www.deezer.com/artist/${id}`,
};

const PROVIDER_LABEL: Record<string, string> = {
  musicbrainz: 'MusicBrainz',
  deezer: 'Deezer',
};

export default function Library() {
  const { toast } = useToast();
  const queryClient = useQueryClient();
  const [filter, setFilter] = useState('');
  const [debouncedFilter, setDebouncedFilter] = useState('');
  const [selecting, setSelecting] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
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

  const toggleSelect = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const rowProps = (artist: Artist) => ({
    selecting,
    selected: selected.has(artist.id),
    onSelect: () => toggleSelect(artist.id),
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

      {sort === 'az' && <AlphabetRail activeLetters={activeLetters} />}

      <div className={sort === 'az' ? 'pr-5' : ''}>
        {debouncedFilter && filteredArtists.length === 0 && (
          <p className="text-zinc-500 text-sm text-center py-6">No matches</p>
        )}
        {sort === 'recent' && (
          <div className="space-y-1">
            {recentArtists.map((artist) => (
              <ArtistRow
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
              <p className="text-[11px] font-semibold text-zinc-500 uppercase tracking-wider mt-3 mb-1 first:mt-0 scroll-mt-4">
                {letter}
              </p>
            )}
            <div className="space-y-1">
              {group.map((artist) => (
                <ArtistRow
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
        <div className="fixed bottom-4 left-1/2 -translate-x-1/2 z-40 flex items-center gap-2 bg-zinc-800 border border-zinc-700 rounded-xl px-3 py-2 shadow-xl shadow-black/40">
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
    </div>
  );
}

function ArtistRow({
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
  onSelect?: () => void;
}) {
  const inner = (
    <>
      {selecting && (
        <input
          type="checkbox"
          checked={!!selected}
          onChange={onSelect}
          onClick={(e) => e.stopPropagation()}
          className="accent-emerald-500 shrink-0"
        />
      )}
      <div className="w-11 h-11 rounded-full bg-zinc-700 overflow-hidden shrink-0">
        {artist.image_url ? (
          <img src={artist.image_url} alt={artist.name} className="w-full h-full object-cover" />
        ) : (
          <div className="w-full h-full flex items-center justify-center text-zinc-500">
            <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" /></svg>
          </div>
        )}
      </div>
      <div className="flex-1 min-w-0">
        <p className="font-medium text-sm truncate">{artist.name}</p>
        {isTrackMatch && matchCount ? (
          <p className="text-[11px] text-zinc-500 mt-0.5">{matchCount} matching track{matchCount > 1 ? 's' : ''}</p>
        ) : (
          <div className="mt-0.5">
            {addedLabel && <p className="text-[11px] text-zinc-500 mb-0.5">Added {addedLabel}</p>}
            <ProgressBar owned={artist.owned_tracks ?? 0} total={artist.total_tracks ?? 0} />
          </div>
        )}
      </div>
      {artist.watch_new_releases && (
        <svg className="w-3.5 h-3.5 text-emerald-500 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z" /><circle cx="12" cy="12" r="3" />
        </svg>
      )}
      {!selecting && PROVIDER_ARTIST_URL[artist.provider] && (
        <button
          onClick={(e) => {
            e.preventDefault();
            e.stopPropagation();
            window.open(PROVIDER_ARTIST_URL[artist.provider](artist.provider_id), '_blank', 'noopener');
          }}
          className="text-zinc-600 hover:text-zinc-300 transition-colors shrink-0"
          title={`View on ${PROVIDER_LABEL[artist.provider] ?? artist.provider}`}
        >
          <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" /><polyline points="15 3 21 3 21 9" /><line x1="10" y1="14" x2="21" y2="3" />
          </svg>
        </button>
      )}
      {artist.provider === 'local' && (
        <span className="px-2 py-0.5 rounded text-[10px] font-medium uppercase shrink-0 bg-amber-900/50 text-amber-400">
          not linked
        </span>
      )}
      {artist.orphaned && (
        <span className="px-2 py-0.5 rounded text-[10px] font-medium uppercase shrink-0 bg-red-900/50 text-red-400">
          orphaned
        </span>
      )}
      <StatusBadge status={artist.status} />
      {!selecting && (
        <svg className="w-4 h-4 text-zinc-600 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m9 18 6-6-6-6" /></svg>
      )}
    </>
  );

  if (selecting) {
    return (
      <button
        onClick={onSelect}
        className={`w-full flex items-center gap-3 rounded-lg p-2.5 text-left transition-colors ${
          selected ? 'bg-emerald-900/30 ring-1 ring-emerald-800/60' : 'bg-zinc-800/40 active:bg-zinc-800'
        }`}
      >
        {inner}
      </button>
    );
  }

  return (
    <Link
      to={`/artist/${artist.id}`}
      className="flex items-center gap-3 bg-zinc-800/40 rounded-lg p-2.5 active:bg-zinc-800 transition-colors"
    >
      {inner}
    </Link>
  );
}

function StatusBadge({ status }: { status: string }) {
  if (status === 'partial' || status === 'watched') return null;

  return (
    <span className="px-2 py-0.5 rounded text-[10px] font-medium uppercase shrink-0 bg-green-900/50 text-green-400">
      owned
    </span>
  );
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
