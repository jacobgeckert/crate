import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react';
import { api } from '../api/client';

export interface PlayerTrack {
  id: number;
  title: string;
  artist?: string;
  album?: string;
  coverUrl?: string;
}

interface PlayerState {
  current: PlayerTrack | null;
  hasNext: boolean;
  hasPrev: boolean;
  play: (track: PlayerTrack, queue?: PlayerTrack[]) => void;
  next: () => void;
  prev: () => void;
  close: () => void;
}

const PlayerContext = createContext<PlayerState | null>(null);

export function usePlayer(): PlayerState {
  const ctx = useContext(PlayerContext);
  if (!ctx) throw new Error('usePlayer must be used inside PlayerProvider');
  return ctx;
}

export function PlayerProvider({ children }: { children: ReactNode }) {
  const [queue, setQueue] = useState<PlayerTrack[]>([]);
  const [index, setIndex] = useState(0);

  const play = useCallback((track: PlayerTrack, q?: PlayerTrack[]) => {
    const list = q && q.length > 0 ? q : [track];
    const at = list.findIndex((t) => t.id === track.id);
    setQueue(list);
    setIndex(at >= 0 ? at : 0);
  }, []);

  const next = useCallback(() => setIndex((i) => Math.min(i + 1, queue.length - 1)), [queue.length]);
  const prev = useCallback(() => setIndex((i) => Math.max(i - 1, 0)), []);
  const close = useCallback(() => setQueue([]), []);

  const current = queue.length > 0 ? queue[Math.min(index, queue.length - 1)] : null;

  const state = useMemo<PlayerState>(() => ({
    current,
    hasNext: index < queue.length - 1,
    hasPrev: index > 0,
    play, next, prev, close,
  }), [current, index, queue.length, play, next, prev, close]);

  return (
    <PlayerContext.Provider value={state}>
      {children}
      <PlayerBar />
    </PlayerContext.Provider>
  );
}

function PlayerBar() {
  const { current, hasNext, hasPrev, next, prev, close } = usePlayer();
  if (!current) return null;

  return (
    <>
      {/* Page-bottom spacer so the fixed bar never covers content */}
      <div className="h-16" />
      <div className="fixed left-0 right-0 bottom-[calc(3.5rem+env(safe-area-inset-bottom,0px))] md:bottom-0 z-50 bg-zinc-900 border-t border-zinc-800 px-3 py-2 flex items-center gap-3">
        {current.coverUrl ? (
          <img src={current.coverUrl} alt="" className="w-10 h-10 rounded object-cover shrink-0" />
        ) : (
          <div className="w-10 h-10 rounded bg-zinc-800 flex items-center justify-center shrink-0">
            <svg className="w-5 h-5 text-zinc-600" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" />
            </svg>
          </div>
        )}
        <div className="min-w-0 w-32 sm:w-48 shrink-0">
          <p className="text-sm font-medium truncate">{current.title}</p>
          <p className="text-[11px] text-zinc-500 truncate">
            {[current.artist, current.album].filter(Boolean).join(' · ')}
          </p>
        </div>
        <button
          onClick={prev}
          disabled={!hasPrev}
          className="shrink-0 text-zinc-400 disabled:opacity-20 active:text-white transition-colors"
          title="Previous"
        >
          <svg className="w-5 h-5" viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M19 20 9 12l10-8v16Z" /><path d="M5 4v16" />
          </svg>
        </button>
        <audio
          key={current.id}
          src={api.trackStreamUrl(current.id)}
          controls
          autoPlay
          onEnded={hasNext ? next : undefined}
          className="flex-1 min-w-0 h-9"
        />
        <button
          onClick={next}
          disabled={!hasNext}
          className="shrink-0 text-zinc-400 disabled:opacity-20 active:text-white transition-colors"
          title="Next"
        >
          <svg className="w-5 h-5" viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="m5 4 10 8-10 8V4Z" /><path d="M19 4v16" />
          </svg>
        </button>
        <button
          onClick={close}
          className="shrink-0 text-zinc-500 active:text-white transition-colors"
          title="Close player"
        >
          <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M18 6 6 18" /><path d="m6 6 12 12" />
          </svg>
        </button>
      </div>
    </>
  );
}
