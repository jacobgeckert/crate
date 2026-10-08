import { useState, useCallback } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { ActivityLog } from '../types/index';

const ACTIVITY_PAGE_SIZE = 50;

// Friendly labels for known action types; anything newer than this map falls
// back to humanized snake_case in the chip label.
const ACTION_LABELS: Record<string, string> = {
  search_started: 'Search started',
  download_started: 'Download started',
  download_complete: 'Download complete',
  download_failed: 'Download failed',
  track_rejected: 'Track rejected',
  upload_committed: 'Upload committed',
  album_refresh: 'Album refresh',
  album_edition: 'Edition change',
  album_split: 'Album split',
  release_add: 'Release added',
  discography_sync: 'Discography sync',
  sync_failed: 'Sync failed',
  library_import: 'Library import',
};

function actionLabel(action: string): string {
  return ACTION_LABELS[action] ?? action.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase());
}

export default function Activity() {
  const [extraActivity, setExtraActivity] = useState<ActivityLog[]>([]);
  const [activityOffset, setActivityOffset] = useState(ACTIVITY_PAGE_SIZE);
  const [loadingMoreActivity, setLoadingMoreActivity] = useState(false);
  const [actionFilter, setActionFilter] = useState('');

  const { data: activityData } = useQuery({
    queryKey: ['activity', actionFilter],
    queryFn: () => api.listActivity(ACTIVITY_PAGE_SIZE, 0, actionFilter),
    refetchInterval: 10_000,
  });

  const activityItems = [...(activityData?.items || []), ...extraActivity];
  const activityTotal = activityData?.total ?? 0;
  const activityActions = activityData?.actions ?? [];

  // Changing the filter restarts pagination for the new subset.
  const selectAction = (action: string) => {
    if (action === actionFilter) return;
    setActionFilter(action);
    setExtraActivity([]);
    setActivityOffset(ACTIVITY_PAGE_SIZE);
  };

  const loadMoreActivity = useCallback(async () => {
    if (loadingMoreActivity || activityOffset >= activityTotal) return;
    setLoadingMoreActivity(true);
    try {
      const data = await api.listActivity(ACTIVITY_PAGE_SIZE, activityOffset, actionFilter);
      setExtraActivity((prev) => [...prev, ...(data.items || [])]);
      setActivityOffset((prev) => prev + ACTIVITY_PAGE_SIZE);
    } finally {
      setLoadingMoreActivity(false);
    }
  }, [activityOffset, activityTotal, loadingMoreActivity, actionFilter]);

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-lg font-bold">Activity</h2>
      </div>
      {activityActions.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-3">
          {['', ...activityActions].map((action) => (
            <button
              key={action || 'all'}
              onClick={() => selectAction(action)}
              className={`px-2.5 py-1 rounded-full text-[11px] font-medium transition-colors ${
                actionFilter === action
                  ? 'bg-zinc-200 text-zinc-900'
                  : 'bg-zinc-800/60 text-zinc-500 border border-zinc-700 active:bg-zinc-800'
              }`}
            >
              {action ? actionLabel(action) : 'All'}
            </button>
          ))}
        </div>
      )}
      <ActivityList
        activity={activityItems}
        total={activityTotal}
        offset={activityOffset}
        loadingMore={loadingMoreActivity}
        onLoadMore={loadMoreActivity}
        filtered={actionFilter !== ''}
      />
    </div>
  );
}

function ActivityList({ activity, total, offset, loadingMore, onLoadMore, filtered }: {
  activity: ActivityLog[];
  total: number;
  offset: number;
  loadingMore: boolean;
  onLoadMore: () => void;
  filtered: boolean;
}) {
  if (!activity.length) {
    return (
      <div className="text-center py-16">
        <svg className="w-12 h-12 mx-auto text-zinc-700 mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
          <path d="M12 8v4l3 3" /><circle cx="12" cy="12" r="10" />
        </svg>
        <p className="text-zinc-500">{filtered ? 'No activity of this type yet' : 'No activity yet'}</p>
      </div>
    );
  }

  const actionIcons: Record<string, string> = {
    search_started: 'text-blue-400',
    download_started: 'text-blue-400',
    download_complete: 'text-green-400',
    download_failed: 'text-red-400',
    track_rejected: 'text-red-400',
    upload_committed: 'text-green-400',
    release_add: 'text-green-400',
    album_refresh: 'text-blue-400',
    album_edition: 'text-violet-400',
    album_split: 'text-violet-400',
    discography_sync: 'text-blue-400',
    sync_failed: 'text-red-400',
    library_import: 'text-amber-400',
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
