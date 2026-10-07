import { useState, useCallback } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { ActivityLog } from '../types/index';

const ACTIVITY_PAGE_SIZE = 50;

export default function Activity() {
  const [extraActivity, setExtraActivity] = useState<ActivityLog[]>([]);
  const [activityOffset, setActivityOffset] = useState(ACTIVITY_PAGE_SIZE);
  const [loadingMoreActivity, setLoadingMoreActivity] = useState(false);

  const { data: activityData } = useQuery({
    queryKey: ['activity'],
    queryFn: () => api.listActivity(ACTIVITY_PAGE_SIZE, 0),
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

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-lg font-bold">Activity</h2>
      </div>
      <ActivityList
        activity={activityItems}
        total={activityTotal}
        offset={activityOffset}
        loadingMore={loadingMoreActivity}
        onLoadMore={loadMoreActivity}
      />
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
