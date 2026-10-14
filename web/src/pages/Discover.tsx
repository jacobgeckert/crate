import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import { useToast } from '../components/Toast';
import type { Suggestion } from '../types/index';

export default function Discover() {
  const queryClient = useQueryClient();
  const { toast } = useToast();

  const { data, isLoading } = useQuery({
    queryKey: ['discover'],
    queryFn: () => api.getDiscover(),
  });

  const refresh = useMutation({
    mutationFn: () => api.getDiscover(true),
    onSuccess: (d) => {
      queryClient.setQueryData(['discover'], d);
      const n = d.suggestions?.length ?? 0;
      toast(n > 0 ? `Found ${n} suggestion(s)` : 'No suggestions right now', n > 0 ? 'success' : 'error');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const suggestions = data?.suggestions ?? [];

  return (
    <div>
      <div className="flex items-center justify-between mb-1">
        <h1 className="text-xl font-bold">Discover</h1>
        {data?.configured && (
          <button
            onClick={() => refresh.mutate()}
            disabled={refresh.isPending}
            className="text-xs text-zinc-400 bg-zinc-800 rounded-lg px-3 py-1.5 active:bg-zinc-700 transition-colors disabled:opacity-50"
          >
            {refresh.isPending ? 'Refreshing…' : 'Refresh'}
          </button>
        )}
      </div>
      <p className="text-sm text-zinc-500 mb-4">Artists similar to ones you watch, via Last.fm</p>

      {isLoading && (
        <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6 gap-3">
          {[...Array(10)].map((_, i) => (
            <div key={i} className="rounded-lg overflow-hidden bg-zinc-800/40 animate-pulse">
              <div className="aspect-square bg-zinc-800" />
              <div className="p-2 space-y-1.5">
                <div className="h-3 bg-zinc-800 rounded w-3/4" />
                <div className="h-2.5 bg-zinc-800 rounded w-1/2" />
              </div>
            </div>
          ))}
        </div>
      )}

      {!isLoading && data && !data.configured && (
        <div className="text-center py-12 text-zinc-500">
          <p className="text-sm">Artist suggestions aren't set up yet.</p>
          <p className="text-xs mt-1 text-zinc-600">
            Add a Last.fm API key in{' '}
            <Link to="/settings" className="text-zinc-300 underline">Settings → Discovery</Link>.
          </p>
        </div>
      )}

      {!isLoading && data?.configured && suggestions.length === 0 && (
        <div className="text-center py-12 text-zinc-500">
          <p className="text-sm">No suggestions right now.</p>
          <p className="text-xs mt-1 text-zinc-600">
            Suggestions are built from artists similar to ones you already watch.
          </p>
        </div>
      )}

      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6 gap-3">
        {suggestions.map((s) => (
          <SuggestionCard key={s.mbid ?? s.name} suggestion={s} />
        ))}
      </div>
    </div>
  );
}

function SuggestionCard({ suggestion: s }: { suggestion: Suggestion }) {
  const [expanded, setExpanded] = useState(false);
  const to = s.mbid
    ? `/browse/artist/${s.mbid}?provider=musicbrainz`
    : `/search?q=${encodeURIComponent(s.name)}`;

  return (
    <Link to={to} className="block">
      <div className="rounded-lg overflow-hidden transition-colors bg-zinc-800/40 hover:bg-zinc-800/70">
        <div className="relative aspect-square bg-zinc-700">
          {s.image_url ? (
            <img src={s.image_url} alt={s.name} className="w-full h-full object-cover" loading="lazy" />
          ) : (
            <div className="w-full h-full flex items-center justify-center text-zinc-500">
              <svg className="w-10 h-10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" /></svg>
            </div>
          )}
        </div>
        <div className="p-2">
          <p className="text-xs font-medium truncate" title={s.name}>{s.name}</p>
          <p
            role="button"
            tabIndex={0}
            onClick={(e) => { e.preventDefault(); e.stopPropagation(); setExpanded((v) => !v); }}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); e.stopPropagation(); setExpanded((v) => !v); } }}
            className={`text-[10px] text-zinc-500 mt-0.5 cursor-pointer ${expanded ? '' : 'line-clamp-2'}`}
            title={expanded ? 'Show less' : s.artists.join(', ')}
          >
            because you watch {s.artists.join(', ')}
          </p>
        </div>
      </div>
    </Link>
  );
}
