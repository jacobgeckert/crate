import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';

export interface PlayerTrack {
  id: number;
  title: string;
  artist?: string;
  album?: string;
  coverUrl?: string;
  hasLyrics?: boolean;
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

const STORAGE_KEY = 'crate:player';

interface PersistedPlayer {
  queue: PlayerTrack[];
  index: number;
  time: number;
  playing: boolean;
  volume: number;
  muted: boolean;
}

// loadPersisted returns the player snapshot saved across page loads, so a
// refresh can resume the queue where it left off.
function loadPersisted(): PersistedPlayer | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as PersistedPlayer;
    if (!Array.isArray(p.queue) || p.queue.length === 0) return null;
    return { ...p, index: Math.min(Math.max(p.index || 0, 0), p.queue.length - 1) };
  } catch {
    return null;
  }
}

// writePersisted merges fields into the stored snapshot — the provider owns
// queue/index, the bar owns position/volume/playing.
function writePersisted(patch: Partial<PersistedPlayer>) {
  const p = loadPersisted();
  if (!p) return;
  localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...p, ...patch }));
}

export function PlayerProvider({ children }: { children: ReactNode }) {
  const [queue, setQueue] = useState<PlayerTrack[]>(() => loadPersisted()?.queue ?? []);
  const [index, setIndex] = useState(() => loadPersisted()?.index ?? 0);

  // Keep the snapshot's queue/index current. A track change resets the saved
  // position; a same-track write (e.g. the mount that restored state) keeps it.
  useEffect(() => {
    if (queue.length === 0) {
      localStorage.removeItem(STORAGE_KEY);
      return;
    }
    const prev = loadPersisted();
    const cur = queue[Math.min(index, queue.length - 1)];
    const sameTrack = prev?.queue[prev.index]?.id === cur?.id;
    localStorage.setItem(STORAGE_KEY, JSON.stringify({
      volume: 1, muted: false, time: 0, playing: true,
      ...prev,
      queue, index,
      ...(sameTrack ? {} : { time: 0, playing: true }),
    }));
  }, [queue, index]);

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
  const [playing, setPlaying] = useState(false);
  const [time, setTime] = useState(0);
  const [duration, setDuration] = useState(0);
  const [volume, setVolume] = useState(() => loadPersisted()?.volume ?? 1);
  const [muted, setMuted] = useState(() => loadPersisted()?.muted ?? false);
  const [expanded, setExpanded] = useState(false);
  const raf = useRef(0);
  const restored = useRef(false);
  const lastSave = useRef(0);

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
    const bar = e.currentTarget;
    const a = audioRef.current;
    if (!bar || !a || !duration) return;
    const rect = bar.getBoundingClientRect();
    const frac = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
    a.currentTime = frac * duration;
    setTime(a.currentTime);
  };

  const togglePlay = () => {
    const a = audioRef.current;
    if (!a) return;
    if (a.paused) a.play(); else a.pause();
  };

  const toggleMute = () => {
    const a = audioRef.current;
    if (!a) return;
    a.muted = !muted;
    setMuted(!muted);
    writePersisted({ muted: !muted });
  };

  const setVol = (v: number) => {
    const a = audioRef.current;
    if (a) { a.volume = v; a.muted = false; }
    setVolume(v);
    setMuted(false);
    writePersisted({ volume: v, muted: false });
  };

  // Snapshot the position on unload so a refresh resumes in place, and save
  // periodically via onTimeUpdate as a fallback.
  useEffect(() => {
    const save = () => {
      const a = audioRef.current;
      if (a) writePersisted({ time: a.currentTime, playing: !a.paused, volume, muted });
    };
    window.addEventListener('pagehide', save);
    return () => window.removeEventListener('pagehide', save);
  }, [volume, muted]);

  if (!current) return null;

  const frac = duration > 0 ? Math.min(1, time / duration) : 0;

  return (
    <>
      {/* Page-bottom spacer so the fixed bar never covers content */}
      <div className="h-16" />
      <div className="fixed left-0 right-0 bottom-[calc(3.5rem+env(safe-area-inset-bottom,0px))] md:bottom-0 z-50 bg-zinc-900/95 backdrop-blur-sm border-t border-zinc-800 px-3 py-2">
        <div className="flex items-center gap-3">
          <button
            onClick={() => setExpanded(true)}
            className="flex items-center gap-3 min-w-0 w-40 sm:w-56 shrink-0 text-left rounded-lg px-1.5 -ml-1.5 py-1 cursor-pointer hover:bg-zinc-800/70 transition-colors"
            title="Expand player"
          >
            {current.coverUrl ? (
              <img src={current.coverUrl} alt="" className="w-10 h-10 rounded object-cover shrink-0" />
            ) : (
              <div className="w-10 h-10 rounded bg-zinc-800 flex items-center justify-center shrink-0">
                <svg className="w-5 h-5 text-zinc-600" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" />
                </svg>
              </div>
            )}
            <span className="min-w-0 flex-1">
              <p className="text-sm font-semibold text-white truncate">{current.title}</p>
              <p className="text-[11px] text-zinc-400 truncate">
                {[current.artist, current.album].filter(Boolean).join(' · ')}
              </p>
            </span>
            <svg className="w-4 h-4 text-zinc-500 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="m18 15-6-6-6 6" />
            </svg>
          </button>
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
            onClick={togglePlay}
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
            onClick={toggleMute}
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
            onChange={(e) => setVol(Number(e.target.value))}
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
      </div>
      {expanded && (
        <FullPlayer
          current={current}
          playing={playing}
          time={time}
          duration={duration}
          frac={frac}
          volume={volume}
          muted={muted}
          hasNext={hasNext}
          hasPrev={hasPrev}
          onSeek={seek}
          onTogglePlay={togglePlay}
          onToggleMute={toggleMute}
          onVolume={setVol}
          onNext={next}
          onPrev={prev}
          onCollapse={() => setExpanded(false)}
        />
      )}
      <audio
        key={current.id}
        ref={audioRef}
        src={api.trackStreamUrl(current.id)}
        autoPlay
        onPlay={() => { setPlaying(true); writePersisted({ playing: true }); }}
        onPause={() => {
          setPlaying(false);
          writePersisted({ playing: false, time: audioRef.current?.currentTime ?? 0 });
        }}
        onTimeUpdate={(e) => {
          const now = Date.now();
          if (now - lastSave.current < 1500) return;
          lastSave.current = now;
          const a = e.currentTarget;
          writePersisted({ time: a.currentTime, playing: !a.paused });
        }}
        onLoadedMetadata={(e) => {
          const a = e.currentTarget;
          setDuration(a.duration);
          // On the first track after a page load, resume position/playing/volume
          // from the snapshot — subsequent tracks start fresh.
          if (!restored.current) {
            restored.current = true;
            const p = loadPersisted();
            if (p && p.queue[p.index]?.id === current.id) {
              if (p.time > 0 && p.time < a.duration) a.currentTime = p.time;
              a.volume = p.muted ? 0 : p.volume;
              a.muted = p.muted;
              if (!p.playing) a.pause();
              return;
            }
          }
          a.volume = muted ? 0 : volume;
          a.muted = muted;
        }}
        onEnded={hasNext ? next : undefined}
        className="hidden"
      />
    </>
  );
}

// FullPlayer is the expanded now-playing overlay — big art over a blurred
// cover backdrop, same controls as the mini bar.
function FullPlayer({
  current, playing, time, duration, frac, volume, muted, hasNext, hasPrev,
  onSeek, onTogglePlay, onToggleMute, onVolume, onNext, onPrev, onCollapse,
}: {
  current: PlayerTrack;
  playing: boolean;
  time: number;
  duration: number;
  frac: number;
  volume: number;
  muted: boolean;
  hasNext: boolean;
  hasPrev: boolean;
  onSeek: (e: React.PointerEvent<HTMLDivElement>) => void;
  onTogglePlay: () => void;
  onToggleMute: () => void;
  onVolume: (v: number) => void;
  onNext: () => void;
  onPrev: () => void;
  onCollapse: () => void;
}) {
  const [showLyrics, setShowLyrics] = useState(false);
  return (
    <div className="fixed inset-0 z-[70] bg-zinc-950 flex flex-col">
      {current.coverUrl && (
        <div
          className="absolute inset-0 bg-cover bg-center blur-3xl scale-125 opacity-30 pointer-events-none"
          style={{ backgroundImage: `url(${current.coverUrl})` }}
        />
      )}
      <div className="relative flex-1 flex flex-col px-6 pt-[max(0.75rem,env(safe-area-inset-top))] pb-[max(1rem,env(safe-area-inset-bottom))] max-w-md w-full mx-auto">
        <div className="flex items-center justify-between mb-2">
          <button
            onClick={onCollapse}
            className="p-2 -ml-2 text-zinc-400 active:text-white transition-colors"
            title="Collapse player"
          >
            <svg className="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="m6 9 6 6 6-6" />
            </svg>
          </button>
          <p className="text-[11px] uppercase tracking-widest text-zinc-500">Now playing</p>
          {current.hasLyrics ? (
            <button
              onClick={() => setShowLyrics((v) => !v)}
              className={`p-2 -mr-2 transition-colors ${showLyrics ? 'text-emerald-400' : 'text-zinc-400 active:text-white'}`}
              title={showLyrics ? 'Hide lyrics' : 'Lyrics'}
            >
              <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M12 2a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z" /><path d="M19 10v1a7 7 0 0 1-14 0v-1" /><line x1="12" y1="19" x2="12" y2="22" />
              </svg>
            </button>
          ) : (
            <span className="w-9" />
          )}
        </div>

        <div className="flex-1 relative flex items-center justify-center min-h-0">
          {showLyrics ? (
            <LyricsView trackId={current.id} time={time} />
          ) : current.coverUrl ? (
            <img
              src={current.coverUrl}
              alt=""
              className="max-h-full max-w-full aspect-square object-cover rounded-2xl shadow-2xl shadow-black/60"
            />
          ) : (
            <div className="w-full aspect-square rounded-2xl bg-zinc-800 flex items-center justify-center text-zinc-600 shadow-2xl shadow-black/60">
              <svg className="w-24 h-24" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M9 18V5l12-2v13" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="16" r="3" /></svg>
            </div>
          )}
        </div>

        <div className="mt-6 min-w-0 text-center">
          <p className="text-xl font-bold text-white truncate">{current.title}</p>
          <p className="text-sm text-zinc-400 truncate mt-1">
            {[current.artist, current.album].filter(Boolean).join(' · ')}
          </p>
        </div>

        <div className="mt-4">
          <div
            onPointerDown={(e) => { e.currentTarget.setPointerCapture(e.pointerId); onSeek(e); }}
            onPointerMove={(e) => { if (e.buttons === 1) onSeek(e); }}
            className="h-6 flex items-center cursor-pointer touch-none"
          >
            <div className="relative w-full h-1.5 bg-zinc-700 rounded-full">
              <div className="absolute inset-y-0 left-0 bg-emerald-400 rounded-full" style={{ width: `${frac * 100}%` }}>
                <div className="absolute right-0 top-1/2 -translate-y-1/2 translate-x-1/2 w-3.5 h-3.5 bg-white rounded-full shadow" />
              </div>
            </div>
          </div>
          <div className="flex justify-between text-[11px] text-zinc-500 tabular-nums mt-1">
            <span>{fmtTime(time)}</span>
            <span>{fmtTime(duration)}</span>
          </div>
        </div>

        <div className="flex items-center justify-center gap-8 mt-2">
          <button
            onClick={onPrev}
            disabled={!hasPrev}
            className="text-zinc-300 disabled:opacity-20 active:text-white transition-colors"
            title="Previous"
          >
            <svg className="w-8 h-8" viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M19 20 9 12l10-8v16Z" /><path d="M5 4v16" />
            </svg>
          </button>
          <button
            onClick={onTogglePlay}
            className="w-16 h-16 rounded-full bg-white text-zinc-900 flex items-center justify-center active:scale-95 transition-transform"
            title={playing ? 'Pause' : 'Play'}
          >
            {playing ? (
              <svg className="w-7 h-7" viewBox="0 0 24 24" fill="currentColor"><path d="M6 4h4v16H6zM14 4h4v16h-4z" /></svg>
            ) : (
              <svg className="w-7 h-7 ml-1" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z" /></svg>
            )}
          </button>
          <button
            onClick={onNext}
            disabled={!hasNext}
            className="text-zinc-300 disabled:opacity-20 active:text-white transition-colors"
            title="Next"
          >
            <svg className="w-8 h-8" viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="m5 4 10 8-10 8V4Z" /><path d="M19 4v16" />
            </svg>
          </button>
        </div>

        <div className="flex items-center gap-3 mt-4">
          <button
            onClick={onToggleMute}
            className="text-zinc-400 active:text-white transition-colors"
            title={muted ? 'Unmute' : 'Mute'}
          >
            {muted || volume === 0 ? (
              <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M11 5 6 9H2v6h4l5 4V5Z" /><line x1="22" y1="9" x2="16" y2="15" /><line x1="16" y1="9" x2="22" y2="15" /></svg>
            ) : (
              <svg className="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M11 5 6 9H2v6h4l5 4V5Z" /><path d="M15.54 8.46a5 5 0 0 1 0 7.07" /></svg>
            )}
          </button>
          <input
            type="range"
            min={0}
            max={1}
            step={0.01}
            value={muted ? 0 : volume}
            onChange={(e) => onVolume(Number(e.target.value))}
            className="flex-1 h-1 accent-emerald-400"
            aria-label="Volume"
          />
        </div>
      </div>
    </div>
  );
}

interface LyricLine {
  t: number;
  text: string;
}

const lrcTimeRe = /\[(\d+):(\d+(?:\.\d+)?)\]/g;

// parseLrc flattens [mm:ss.xx] tags into timed lines; lines with multiple
// tags emit one entry per timestamp.
function parseLrc(text: string): LyricLine[] {
  const out: LyricLine[] = [];
  for (const raw of text.split('\n')) {
    const times: number[] = [];
    lrcTimeRe.lastIndex = 0;
    let m: RegExpExecArray | null;
    let consumed = 0;
    while ((m = lrcTimeRe.exec(raw)) !== null) {
      times.push(Number(m[1]) * 60 + parseFloat(m[2]));
      consumed = lrcTimeRe.lastIndex;
    }
    const text = raw.slice(consumed).trim();
    for (const t of times) out.push({ t, text });
  }
  return out.sort((a, b) => a.t - b.t);
}

// LyricsView renders plain lyrics as static text; synced (.lrc) lyrics get
// the active line highlighted and auto-scrolled to center.
function LyricsView({ trackId, time }: { trackId: number; time: number }) {
  const { data, isLoading } = useQuery({
    queryKey: ['track-lyrics', trackId],
    queryFn: () => api.getTrackLyrics(trackId),
  });

  const lines = useMemo(() => (data?.synced ? parseLrc(data.text) : []), [data]);
  const activeIdx = useMemo(() => {
    let idx = -1;
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].t <= time) idx = i; else break;
    }
    return idx;
  }, [lines, time]);
  const activeRef = useRef<HTMLParagraphElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: 0 });
  }, [data]);
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }, [activeIdx]);

  if (isLoading) {
    return (
      <div className="absolute inset-0 flex items-center justify-center">
        <div className="w-8 h-8 rounded-full border-2 border-zinc-600 border-t-emerald-400 animate-spin" />
      </div>
    );
  }
  if (!data) {
    return (
      <div className="absolute inset-0 flex items-center justify-center">
        <p className="text-sm text-zinc-500">No lyrics found for this track</p>
      </div>
    );
  }
  if (!data.synced) {
    return (
      <div ref={scrollRef} className="absolute inset-0 overflow-y-auto">
        <p className="whitespace-pre-wrap text-sm text-zinc-300 text-center leading-relaxed py-4">{data.text}</p>
      </div>
    );
  }
  return (
    <div ref={scrollRef} className="absolute inset-0 overflow-y-auto py-[35vh] text-center">
      {lines.map((l, i) => (
        <p
          key={i}
          ref={i === activeIdx ? activeRef : undefined}
          className={`py-1 transition-colors ${
            i === activeIdx ? 'text-white font-semibold text-base' : 'text-zinc-500 text-sm'
          }`}
        >
          {l.text || '♪'}
        </p>
      ))}
    </div>
  );
}
