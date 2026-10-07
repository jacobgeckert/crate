import { useState, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { formatSpeed } from '../lib/format';
import DetailSheet, { DetailRow } from '../components/DetailSheet';
import type { DownloadQueueItem, DownloadProgress, ActivityLog } from '../types/index';

const ACTIVITY_PAGE_SIZE = 50;

const STATUS_ORDER = ['downloading', 'organizing', 'searching', 'pending', 'failed', 'complete'];
const STATUS_STYLES: Record<string, string> = {
  complete: 'bg-green-900/50 text-green-400',
  pending: 'bg-zinc-700 text-zinc-400',
  searching: 'bg-blue-900/50 text-blue-300',
  downloading: 'bg-blue-900/50 text-blue-400',
  organizing: 'bg-blue-900/50 text-blue-400',
  failed: 'bg-red-900/50 text-red-400',
};
const STATUS_SHORT: Record<string, string> = {
  complete: 'done',
  downloading: 'dl',
};

interface AlbumGroup {
  key: string;
  albumId?: number;
  title: string;
  artist: string;
  cover?: string;
  items: DownloadQueueItem[];
}

// Downloads are ordered newest-first by the backend; Map insertion order
// keeps groups sorted by their most recent queue row.
function groupByAlbum(downloads: DownloadQueueItem[]): AlbumGroup[] {
  const m = new Map<string, AlbumGroup>();
  for (const dl of downloads) {
    const t = dl.track;
    const key = t?.album_id ? `album:${t.album_id}` : `row:${dl.id}`;
    let g = m.get(key);
    if (!g) {
      g = {
        key,
        albumId: t?.album_id,
        title: t?.album_title || 'Unknown album',
        artist: t?.artist_name || '',
        cover: t?.album_cover_url,
        items: [],
      };
      m.set(key, g);
    }
    g.items.push(dl);
  }
  return [...m.values()];
}

export default function Downloads() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [tab, setTab] = useState<'queue' | 'activity'>('queue');
  const [selected, setSelected] = useState<DownloadQueueItem | null>(null);

  const { data: downloads, isLoading } = useQuery({
    queryKey: ['downloads'],
    queryFn: () => api.listDownloads(),
    refetchInterval: 5000,
  });

  const hasActiveDownloads = downloads?.some(
    (d) => d.status === 'downloading' || d.status === 'searching' || d.status === 'pending' || d.status === 'organizing'
  ) ?? false;

  const { data: progress } = useQuery({
    queryKey: ['download-progress'],
    queryFn: api.getDownloadProgress,
    refetchInterval: hasActiveDownloads ? 2000 : false,
    enabled: hasActiveDownloads,
  });

  // slskd's username casing can differ from what the search stored; keys
  // compare lowercased on both sides.
  const progressByKey = useMemo(() => {
    const m = new Map<string, DownloadProgress>();
    for (const p of progress ?? []) m.set(p.key.toLowerCase(), p);
    return m;
  }, [progress]);

  const groups = useMemo(() => groupByAlbum(downloads ?? []), [downloads]);
  const failedTotal = downloads?.filter((d) => d.status === 'failed').length ?? 0;
  const completeTotal = downloads?.filter((d) => d.status === 'complete').length ?? 0;

  const [extraActivity, setExtraActivity] = useState<ActivityLog[]>([]);
  const [activityOffset, setActivityOffset] = useState(ACTIVITY_PAGE_SIZE);
  const [loadingMoreActivity, setLoadingMoreActivity] = useState(false);

  const { data: activityData } = useQuery({
    queryKey: ['activity'],
    queryFn: () => api.listActivity(ACTIVITY_PAGE_SIZE, 0),
    enabled: tab === 'activity',
    refetchInterval: 10_000,
  });

  const activityItems = [...(activityData?.items || []), ...extraActivity];
  const activityTotal = activityData?.total ?? 0;

  const loadMoreActivity = useCallback(async () => {
    if (loadingMoreActivity || activityOffset >= activityTotal) return;
    setLoadingMoreActivity(true);
    try {
      const data = await api.listActivity(ACTIVITY_PAGE_SIZE, activityOffset);
      setExtraActivity((prev) => [...prev, ...(data.items || [])]);
      setActivityOffset((prev) => prev + ACTIVITY_PAGE_SIZE);
    } finally {
      setLoadingMoreActivity(false);
    }
  }, [activityOffset, activityTotal, loadingMoreActivity]);

  const queue = useMutation({
    mutationFn: api.queueDownloads,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['downloads'] }),
  });

  const retry = useMutation({
    mutationFn: (id: number) => api.retryDownload(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['downloads'] }),
  });

  const dismiss = useMutation({
    mutationFn: (id: number) => api.deleteDownload(id),
    onMutate: async (id) => {
      await queryClient.cancelQueries({ queryKey: ['downloads'] });
      queryClient.setQueryData(['downloads'], (old: DownloadQueueItem[] | undefined) =>
        old?.filter((d) => d.id !== id)
      );
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ['downloads'] }),
  });

  const clearByStatus = useMutation({
    mutationFn: (status: string) => api.clearDownloadsByStatus(status),
    onMutate: async (status) => {
      await queryClient.cancelQueries({ queryKey: ['downloads'] });
      queryClient.setQueryData(['downloads'], (old: DownloadQueueItem[] | undefined) =>
        old?.filter((d) => d.status !== status)
      );
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ['downloads'] }),
  });

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <div className="flex items-center gap-3">
          <h2 className="text-lg font-bold">Downloads</h2>
          <div className="flex bg-zinc-800 rounded-lg p-0.5">
            <button
              onClick={() => setTab('queue')}
              className={`px-3 py-1 rounded-md text-xs font-medium transition-colors ${
                tab === 'queue' ? 'bg-zinc-700 text-white' : 'text-zinc-400'
              }`}
            >
              Queue
            </button>
            <button
              onClick={() => setTab('activity')}
              className={`px-3 py-1 rounded-md text-xs font-medium transition-colors ${
                tab === 'activity' ? 'bg-zinc-700 text-white' : 'text-zinc-400'
              }`}
            >
              Activity
            </button>
          </div>
        </div>
        {tab === 'queue' && (
          <div className="flex items-center gap-3">
            {failedTotal > 0 && (
              <button
                onClick={() => clearByStatus.mutate('failed')}
                className="text-[11px] text-zinc-500 active:text-red-400 transition-colors"
              >
                Clear failed
              </button>
            )}
            {completeTotal > 0 && (
              <button
                onClick={() => clearByStatus.mutate('complete')}
                className="text-[11px] text-zinc-500 active:text-red-400 transition-colors"
              >
                Clear complete
              </button>
            )}
            <button
              onClick={() => queue.mutate()}
              disabled={queue.isPending}
              className="px-3 py-1.5 bg-white text-zinc-900 rounded-lg text-xs font-semibold active:scale-[0.97] transition-transform disabled:opacity-50"
            >
              {queue.isPending ? 'Queuing...' : queue.data ? `Queued ${queue.data.queued}` : 'Queue All Wanted'}
            </button>
          </div>
        )}
      </div>

      {tab === 'queue' && (
        <>
          {isLoading && (
            <div className="space-y-1">
              {[...Array(3)].map((_, i) => (
                <div key={i} className="flex items-center gap-2.5 bg-zinc-800/40 rounded-lg px-3 py-2 h-14 animate-pulse" />
              ))}
            </div>
          )}

          {!isLoading && !downloads?.length && (
            <div className="text-center py-16">
              <svg className="w-12 h-12 mx-auto text-zinc-700 mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" /><polyline points="7 10 12 15 17 10" /><line x1="12" y1="15" x2="12" y2="3" />
              </svg>
              <p className="text-zinc-500">No downloads yet</p>
              <p className="text-zinc-600 text-sm mt-1.5">Hit "Queue All Wanted" to start downloading watched tracks</p>
            </div>
          )}

          <div className="space-y-1.5">
            {groups.map((g) => (
              <AlbumDownloadGroup
                key={g.key}
                group={g}
                progressByKey={progressByKey}
                onOpenAlbum={g.albumId ? () => navigate(`/album/${g.albumId}`) : undefined}
                onRetry={retry.mutate}
                onDismiss={dismiss.mutate}
                onRetryAll={() => g.items.filter((i) => i.status === 'failed').forEach((i) => retry.mutate(i.id))}
                onDismissAll={() => g.items.forEach((i) => dismiss.mutate(i.id))}
                onSelect={setSelected}
              />
            ))}
          </div>
        </>
      )}

      {tab === 'activity' && (
        <ActivityList
          activity={activityItems}
          total={activityTotal}
          offset={activityOffset}
          loadingMore={loadingMoreActivity}
          onLoadMore={loadMoreActivity}
        />
      )}

      <DetailSheet
        open={!!selected}
        onClose={() => setSelected(null)}
        title={selected?.track?.title ?? `Download #${selected?.id}`}
      >
        {selected && (
          <div className="space-y-0.5">
            {selected.track?.artist_name && <DetailRow label="Artist">{selected.track.artist_name}</DetailRow>}
            {selected.track?.album_title && <DetailRow label="Album">{selected.track.album_title}</DetailRow>}
            <DetailRow label="Status"><StatusBadge status={selected.status} /></DetailRow>
            <DetailRow label="Attempts">{selected.attempts} / 4{selected.attempts < 4 && selected.status !== 'complete' ? ` (${4 - selected.attempts} left)` : ''}</DetailRow>
            <DetailRow label="Queued">{new Date(selected.created_at).toLocaleString()}</DetailRow>
            {selected.last_attempt && <DetailRow label="Last Attempt">{new Date(selected.last_attempt).toLocaleString()}</DetailRow>}
            {selected.next_retry_at && <DetailRow label="Retries At">{new Date(selected.next_retry_at).toLocaleString()}</DetailRow>}
            {selected.error && (
              <div className="mt-3 p-3 bg-red-950/30 border border-red-900/50 rounded-lg">
                <p className="text-[11px] text-red-400 uppercase tracking-wider mb-1">Error</p>
                <p className="text-sm text-red-300 break-words">{selected.error}</p>
              </div>
            )}
          </div>
        )}
      </DetailSheet>
    </div>
  );
}

function ActivityList({ activity, total, offset, loadingMore, onLoadMore }: {
  activity: ActivityLog[];
  total: number;
  offset: number;
  loadingMore: boolean;
  onLoadMore: () => void;
}) {
  if (!activity.length) {
    return (
      <div className="text-center py-16">
        <svg className="w-12 h-12 mx-auto text-zinc-700 mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
          <path d="M12 8v4l3 3" /><circle cx="12" cy="12" r="10" />
        </svg>
        <p className="text-zinc-500">No activity yet</p>
      </div>
    );
  }

  const actionIcons: Record<string, string> = {
    search_started: 'text-blue-400',
    download_started: 'text-blue-400',
    download_complete: 'text-green-400',
    download_failed: 'text-red-400',
    file_missing: 'text-amber-400',
  };

  return (
    <div className="space-y-0.5">
      {activity.map((a) => (
        <div key={a.id} className="flex items-start gap-2.5 px-2.5 py-2 rounded-lg">
          <div className={`w-1.5 h-1.5 rounded-full mt-1.5 shrink-0 ${actionIcons[a.action] ? actionIcons[a.action].replace('text-', 'bg-') : 'bg-zinc-600'}`} />
          <div className="flex-1 min-w-0">
            <p className="text-sm truncate">{a.details}</p>
            <p className="text-[10px] text-zinc-600">
              {new Date(a.created_at).toLocaleString()}
            </p>
          </div>
        </div>
      ))}
      {offset < total && (
        <button
          onClick={onLoadMore}
          disabled={loadingMore}
          className="w-full py-2.5 text-sm text-zinc-400 bg-zinc-800/40 rounded-lg active:bg-zinc-800 transition-colors disabled:opacity-50 mt-2"
        >
          {loadingMore ? 'Loading...' : `Load More (${activity.length} of ${total})`}
        </button>
      )}
    </div>
  );
}

function AlbumCover({ url }: { url?: string }) {
  const [failed, setFailed] = useState(false);
  return (
    <div className="w-10 h-10 rounded-md bg-zinc-700/60 overflow-hidden shrink-0 flex items-center justify-center text-zinc-600">
      {url && !failed ? (
        <img src={url} alt="" className="w-full h-full object-cover" onError={() => setFailed(true)} />
      ) : (
        <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><rect x="3" y="3" width="18" height="18" rx="2" /><circle cx="12" cy="12" r="4" /><circle cx="12" cy="12" r="1" /></svg>
      )}
    </div>
  );
}

function AlbumDownloadGroup({ group, progressByKey, onOpenAlbum, onRetry, onDismiss, onRetryAll, onDismissAll, onSelect }: {
  group: AlbumGroup;
  progressByKey: Map<string, DownloadProgress>;
  onOpenAlbum?: () => void;
  onRetry: (id: number) => void;
  onDismiss: (id: number) => void;
  onRetryAll: () => void;
  onDismissAll: () => void;
  onSelect: (dl: DownloadQueueItem) => void;
}) {
  // Auto-expand while something's in flight; a manual toggle wins once made.
  const [manualOpen, setManualOpen] = useState<boolean | null>(null);
  const hasActive = group.items.some((i) => i.status === 'downloading' || i.status === 'searching');
  const open = manualOpen ?? hasActive;

  const counts = useMemo(() => {
    const m = new Map<string, number>();
    for (const i of group.items) m.set(i.status, (m.get(i.status) ?? 0) + 1);
    return m;
  }, [group.items]);

  const sorted = useMemo(
    () => [...group.items].sort((a, b) =>
      (a.track?.disc_number ?? 0) - (b.track?.disc_number ?? 0) ||
      (a.track?.track_number ?? 0) - (b.track?.track_number ?? 0) ||
      (a.track?.title ?? '').localeCompare(b.track?.title ?? '')),
    [group.items]
  );

  const failedCount = counts.get('failed') ?? 0;

  return (
    <div className="bg-zinc-800/40 rounded-lg overflow-hidden">
      <div
        className="flex items-center gap-2.5 px-3 py-2 cursor-pointer active:bg-zinc-800/70 transition-colors"
        onClick={() => setManualOpen(!open)}
      >
        <AlbumCover url={group.cover} />
        <div className="flex-1 min-w-0">
          <p className="text-sm font-medium truncate">{group.title}</p>
          <p className="text-[11px] text-zinc-500 truncate">
            {group.artist}{group.artist ? ' · ' : ''}{group.items.length} {group.items.length === 1 ? 'track' : 'tracks'}
          </p>
        </div>
        <div className="flex items-center gap-1 shrink-0">
          {STATUS_ORDER.filter((s) => counts.has(s)).map((s) => (
            <span key={s} className={`px-1.5 py-0.5 rounded text-[10px] font-medium uppercase ${STATUS_STYLES[s] ?? ''}`}>
              {counts.get(s)} {STATUS_SHORT[s] ?? s}
            </span>
          ))}
        </div>
        <svg className={`w-3.5 h-3.5 text-zinc-500 shrink-0 transition-transform ${open ? 'rotate-180' : ''}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="m6 9 6 6 6-6" />
        </svg>
      </div>

      {open && (
        <div className="border-t border-zinc-700/40">
          {sorted.map((dl) => (
            <QueueTrackRow
              key={dl.id}
              dl={dl}
              prog={dl.slskd_search_id ? progressByKey.get(dl.slskd_search_id.toLowerCase()) : undefined}
              onRetry={onRetry}
              onDismiss={onDismiss}
              onSelect={onSelect}
            />
          ))}
          <div className="flex items-center justify-between px-3 py-1.5 border-t border-zinc-700/40">
            {onOpenAlbum ? (
              <button onClick={onOpenAlbum} className="text-[11px] text-blue-400 active:text-blue-300 transition-colors">
                View album
              </button>
            ) : <span />}
            <div className="flex items-center gap-3">
              {failedCount > 0 && (
                <button onClick={onRetryAll} className="text-[11px] text-zinc-400 active:text-white transition-colors">
                  Retry failed
                </button>
              )}
              <button onClick={onDismissAll} className="text-[11px] text-zinc-500 active:text-red-400 transition-colors">
                Remove all
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function QueueTrackRow({ dl, prog, onRetry, onDismiss, onSelect }: {
  dl: DownloadQueueItem;
  prog?: DownloadProgress;
  onRetry: (id: number) => void;
  onDismiss: (id: number) => void;
  onSelect: (dl: DownloadQueueItem) => void;
}) {
  const num = dl.track?.track_number;
  const disc = dl.track?.disc_number;
  const label = disc && disc > 1 ? `${disc}-${num ?? ''}` : num ? String(num) : '';

  return (
    <div className="px-3 py-2 cursor-pointer active:bg-zinc-800/70 transition-colors" onClick={() => onSelect(dl)}>
      <div className="flex items-center gap-2">
        <span className="w-5 text-right text-[11px] text-zinc-600 tabular-nums shrink-0">{label}</span>
        <p className="text-sm truncate flex-1 min-w-0">{dl.track?.title ?? `Track #${dl.track_id}`}</p>
        <StatusBadge status={dl.status} />
        {dl.status === 'failed' && (
          <button
            onClick={(e) => { e.stopPropagation(); onRetry(dl.id); }}
            className="px-2.5 py-1 bg-zinc-700 rounded text-xs active:bg-zinc-600 transition-colors shrink-0"
          >
            Retry
          </button>
        )}
        <button
          onClick={(e) => { e.stopPropagation(); onDismiss(dl.id); }}
          className="text-zinc-600 active:text-red-400 transition-colors shrink-0"
          title={dl.status === 'downloading' || dl.status === 'searching' ? 'Cancel' : 'Dismiss'}
        >
          <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M18 6 6 18" /><path d="m6 6 12 12" />
          </svg>
        </button>
      </div>
      {prog && (
        <div className="ml-7 mt-1.5">
          <div className="h-1 bg-zinc-700 rounded-full overflow-hidden">
            <div
              className="h-full bg-blue-500 rounded-full transition-all duration-500"
              style={{ width: `${prog.percent_complete}%` }}
            />
          </div>
          <p className="text-[11px] text-zinc-500 mt-1">
            {prog.percent_complete.toFixed(0)}% · {formatSpeed(prog.average_speed_bps)} · {prog.username}
          </p>
        </div>
      )}
      {(dl.error || (dl.status === 'failed' && dl.next_retry_at)) && (
        <p className="ml-7 mt-0.5 text-[11px] truncate">
          {dl.error && <span className="text-red-400">{dl.error}</span>}
          {dl.status === 'failed' && dl.next_retry_at && (
            <span className="text-zinc-600">{dl.error ? ' · ' : ''}retries {new Date(dl.next_retry_at).toLocaleTimeString()}</span>
          )}
        </p>
      )}
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  return (
    <span className={`px-2 py-0.5 rounded text-[10px] font-medium uppercase shrink-0 ${STATUS_STYLES[status] || ''}`}>
      {status}
    </span>
  );
}
