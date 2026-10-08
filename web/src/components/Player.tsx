import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
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

function fmtTime(s: number): string {
  if (!isFinite(s) || s < 0) return '0:00';
  return `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, '0')}`;
}

function PlayerBar() {
  const { current, hasNext, hasPrev, next, prev, close } = usePlayer();
  const audioRef = useRef<HTMLAudioElement>(null);
  const barRef = useRef<HTMLDivElement>(null);
  const [playing, setPlaying] = useState(false);
  const [time, setTime] = useState(0);
  const [duration, setDuration] = useState(0);
  const [volume, setVolume] = useState(1);
  const [muted, setMuted] = useState(false);
  const raf = useRef(0);

  // rAF-driven progress — the audio element's timeupdate only fires ~4Hz,
  // which makes a native-style bar visibly step. This interpolates smoothly.
  useEffect(() => {
    if (!current) return;
    setTime(0);
    setDuration(0);
    const tick = () => {
      const a = audioRef.current;
      if (a) setTime(a.currentTime);
      raf.current = requestAnimationFrame(tick);
    };
    raf.current = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf.current);
  }, [current]);

  const seek = (e: React.PointerEvent<HTMLDivElement>) => {
    const bar = barRef.current;
    const a = audioRef.current;
    if (!bar || !a || !duration) return;
    const rect = bar.getBoundingClientRect();
    const frac = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
    a.currentTime = frac * duration;
    setTime(a.currentTime);
  };

  if (!current) return null;

  const frac = duration > 0 ? Math.min(1, time / duration) : 0;

  return (
    <>
      {/* Page-bottom spacer so the fixed bar never covers content */}
      <div className="h-16" />
      <div className="fixed left-0 right-0 bottom-[calc(3.5rem+env(safe-area-inset-bottom,0px))] md:bottom-0 z-50 bg-zinc-900/95 backdrop-blur-sm border-t border-zinc-800 px-3 py-2">
        <div className="flex items-center gap-3">
          {current.coverUrl ? (
            <img src={current.coverUrl} alt="" className="w-10 h-10 rounded object-cover shrink-0" />
          ) : (
            <div className="w-10 h-10 rounded bg-zinc-800 flex items-center justify-center shrink-0">
              <svg className="w-5 h-5 text-zinc-600" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" />
              </svg>
            </div>
          )}
          <div className="min-w-0 w-36 sm:w-48 shrink-0">
            <p className="text-sm font-semibold text-white truncate">{current.title}</p>
            <p className="text-[11px] text-zinc-400 truncate">
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
          <button
            onClick={() => {
              const a = audioRef.current;
              if (!a) return;
              if (a.paused) a.play(); else a.pause();
            }}
            className="shrink-0 w-8 h-8 rounded-full bg-white text-zinc-900 flex items-center justify-center active:scale-95 transition-transform"
            title={playing ? 'Pause' : 'Play'}
          >
            {playing ? (
              <svg className="w-4 h-4" viewBox="0 0 24 24" fill="currentColor"><path d="M6 4h4v16H6zM14 4h4v16h-4z" /></svg>
            ) : (
              <svg className="w-4 h-4 ml-0.5" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z" /></svg>
            )}
          </button>
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
          <span className="hidden sm:block text-[11px] text-zinc-500 tabular-nums shrink-0 w-9 text-right">{fmtTime(time)}</span>
          <div
            ref={barRef}
            onPointerDown={(e) => { e.currentTarget.setPointerCapture(e.pointerId); seek(e); }}
            onPointerMove={(e) => { if (e.buttons === 1) seek(e); }}
            className="flex-1 min-w-0 h-4 flex items-center cursor-pointer touch-none"
          >
            <div className="relative w-full h-1 bg-zinc-700 rounded-full">
              <div className="absolute inset-y-0 left-0 bg-emerald-400 rounded-full" style={{ width: `${frac * 100}%` }}>
                <div className="absolute right-0 top-1/2 -translate-y-1/2 translate-x-1/2 w-2.5 h-2.5 bg-white rounded-full shadow" />
              </div>
            </div>
          </div>
          <span className="hidden sm:block text-[11px] text-zinc-500 tabular-nums shrink-0 w-9">{fmtTime(duration)}</span>
          <button
            onClick={() => {
              const a = audioRef.current;
              if (!a) return;
              a.muted = !muted;
              setMuted(!muted);
            }}
            className="hidden md:block shrink-0 text-zinc-400 active:text-white transition-colors"
            title={muted ? 'Unmute' : 'Mute'}
          >
            {muted || volume === 0 ? (
              <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M11 5 6 9H2v6h4l5 4V5Z" /><line x1="22" y1="9" x2="16" y2="15" /><line x1="16" y1="9" x2="22" y2="15" /></svg>
            ) : (
              <svg className="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M11 5 6 9H2v6h4l5 4V5Z" /><path d="M15.54 8.46a5 5 0 0 1 0 7.07" /></svg>
            )}
          </button>
          <input
            type="range"
            min={0}
            max={1}
            step={0.01}
            value={muted ? 0 : volume}
            onChange={(e) => {
              const v = Number(e.target.value);
              const a = audioRef.current;
              if (a) { a.volume = v; a.muted = false; }
              setVolume(v);
              setMuted(false);
            }}
            className="hidden md:block w-20 h-1 accent-emerald-400 shrink-0"
            aria-label="Volume"
          />
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
        <audio
          key={current.id}
          ref={audioRef}
          src={api.trackStreamUrl(current.id)}
          autoPlay
          onPlay={() => setPlaying(true)}
          onPause={() => setPlaying(false)}
          onLoadedMetadata={(e) => {
            setDuration(e.currentTarget.duration);
            e.currentTarget.volume = muted ? 0 : volume;
            e.currentTarget.muted = muted;
          }}
          onEnded={hasNext ? next : undefined}
          className="hidden"
        />
      </div>
    </>
  );
}
