import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import { useToast } from '../components/Toast';
import { usePersistedQuery } from '../lib/persistedQuery';
import type { Suggestion, DiscoverResponse } from '../types/index';

// dismissKey mirrors the backend's DismissalKey — MBID when present, else a
// normalized name (letters/digits kept, "the " stripped).
function dismissKey(s: Suggestion): string {
  if (s.mbid) return 'mbid:' + s.mbid.toLowerCase();
  const n = s.name.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, ' ').trim().replace(/\s+/g, ' ').replace(/^the /, '');
  return 'name:' + n;
}

export default function Discover() {
  const queryClient = useQueryClient();
  const { toast } = useToast();

  const { data, isLoading } = usePersistedQuery('discover', ['discover'], () => api.getDiscover());

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
        <div className="space-y-1">
          {[...Array(6)].map((_, i) => (
            <div key={i} className="flex items-center gap-3 bg-zinc-800/40 rounded-lg p-2 animate-pulse">
              <div className="w-12 h-12 rounded bg-zinc-800 shrink-0" />
              <div className="flex-1 space-y-1.5">
                <div className="h-3 bg-zinc-800 rounded w-1/3" />
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

      <div className="space-y-1">
        {suggestions.map((s) => (
          <SuggestionRow key={s.mbid ?? s.name} suggestion={s} />
        ))}
      </div>
    </div>
  );
}

function SuggestionRow({ suggestion: s }: { suggestion: Suggestion }) {
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const dismiss = useMutation({
    mutationFn: () => api.dismissDiscover(dismissKey(s)),
    onSuccess: () => {
      // Keep the original fetch time so a dismissal doesn't restart the weekly cache window.
      const updatedAt = queryClient.getQueryState(['discover'])?.dataUpdatedAt;
      queryClient.setQueryData<DiscoverResponse>(['discover'], (old) =>
        old ? { ...old, suggestions: (old.suggestions ?? []).filter((x) => dismissKey(x) !== dismissKey(s)) } : old,
      { updatedAt });
    },
  });
  const to = s.mbid
    ? `/browse/artist/${s.mbid}?provider=musicbrainz`
    : `/search?q=${encodeURIComponent(s.name)}`;

  return (
    <Link to={to} className="block">
      <div className="flex items-center gap-3 bg-zinc-800/40 hover:bg-zinc-800/70 rounded-lg p-2 transition-colors">
        <div className="w-12 h-12 rounded bg-zinc-700 overflow-hidden shrink-0">
          {s.image_url ? (
            <img src={s.image_url} alt={s.name} className="w-full h-full object-cover" loading="lazy" />
          ) : (
            <div className="w-full h-full flex items-center justify-center text-zinc-500">
              <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" /></svg>
            </div>
          )}
        </div>
        <div className="flex-1 min-w-0">
          <p className="font-medium text-sm truncate" title={s.name}>{s.name}</p>
          <p
            role="button"
            tabIndex={0}
            onClick={(e) => { e.preventDefault(); e.stopPropagation(); setExpanded((v) => !v); }}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); e.stopPropagation(); setExpanded((v) => !v); } }}
            className={`text-[11px] text-zinc-500 mt-0.5 cursor-pointer ${expanded ? '' : 'line-clamp-2'}`}
            title={expanded ? 'Show less' : s.artists.join(', ')}
          >
            Because you listen to {expanded || s.artists.length <= 3
              ? s.artists.join(', ')
              : `${s.artists.slice(0, 3).join(', ')} +${s.artists.length - 3}`}
          </p>
        </div>
        <button
          onClick={(e) => { e.preventDefault(); e.stopPropagation(); dismiss.mutate(); }}
          title="Not interested"
          className="p-1 text-zinc-600 hover:text-zinc-300 shrink-0 transition-colors"
        >
          <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M18 6 6 18" /><path d="m6 6 12 12" />
          </svg>
        </button>
        <svg className="w-4 h-4 text-zinc-600 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="m9 18 6-6-6-6" />
        </svg>
      </div>
    </Link>
  );
}
