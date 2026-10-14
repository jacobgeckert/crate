import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import { useToast } from '../components/Toast';

export default function Discover() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
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

  const watch = useMutation({
    mutationFn: (mbid: string) => api.watchArtist(mbid, { provider: 'musicbrainz' }),
    onSuccess: (_artist, mbid) => {
      queryClient.invalidateQueries({ queryKey: ['artists'] });
      queryClient.setQueryData(['discover'], (d: typeof data) =>
        d ? { ...d, suggestions: d.suggestions.filter((s) => s.mbid !== mbid) } : d);
      toast('Artist added to your library', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const suggestions = data?.suggestions ?? [];

  return (
    <div className="max-w-2xl mx-auto">
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
        <div className="space-y-2">
          {[...Array(4)].map((_, i) => (
            <div key={i} className="h-14 bg-zinc-800/40 rounded-lg animate-pulse" />
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
          <div key={s.mbid ?? s.name} className="flex items-center gap-2.5 bg-zinc-800/40 rounded-lg p-2.5">
            <div className="flex-1 min-w-0">
              <p className="font-medium text-sm truncate">{s.name}</p>
              <p className="text-[11px] text-zinc-500 truncate">
                because you watch {s.artists.slice(0, 3).join(', ')}{s.artists.length > 3 ? ` +${s.artists.length - 3}` : ''}
              </p>
            </div>
            {s.mbid ? (
              <button
                onClick={() => watch.mutate(s.mbid!)}
                disabled={watch.isPending && watch.variables === s.mbid}
                className="text-[11px] font-medium bg-zinc-700 text-zinc-200 rounded-lg px-3 py-1.5 active:bg-zinc-600 transition-colors disabled:opacity-50 shrink-0"
              >
                {watch.isPending && watch.variables === s.mbid ? 'Adding…' : 'Watch'}
              </button>
            ) : (
              <button
                onClick={() => navigate(`/search?q=${encodeURIComponent(s.name)}`)}
                className="text-[11px] font-medium bg-zinc-800 text-zinc-400 rounded-lg px-3 py-1.5 active:bg-zinc-700 transition-colors shrink-0"
              >
                Search
              </button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
