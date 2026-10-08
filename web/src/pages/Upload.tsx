import { useCallback, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { formatFileSize, formatDuration, providerAlbumUrl, providerReleaseUrl } from '../lib/format';
import { useToast } from '../components/Toast';
import ProviderBadge from '../components/ProviderBadge';
import type { AlbumEdition, UploadAlbumRef, UploadBatch, UploadFileItem } from '../types/index';

const ACCEPT = '.mp3,.flac,.wav';

function ConfidenceBadge({ c }: { c: string }) {
  const cls =
    c === 'high'
      ? 'bg-emerald-900/60 text-emerald-300'
      : c === 'medium'
        ? 'bg-amber-900/60 text-amber-300'
        : 'bg-zinc-800 text-zinc-400';
  return <span className={`text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded ${cls}`}>{c}</span>;
}

// Inline drill-down picker for manually overriding a file's match: artist →
// release → track. Used when identification picked the wrong album or found
// none — the file commits onto the chosen library track.
function FileOverridePicker({ onPick }: { onPick: (trackId: number) => void }) {
  const [artistId, setArtistId] = useState<number | null>(null);
  const [albumId, setAlbumId] = useState<number | null>(null);
  const [q, setQ] = useState('');

  const { data: artists } = useQuery({
    queryKey: ['artists'],
    queryFn: api.listArtists,
    staleTime: 60_000,
  });
  const { data: artist } = useQuery({
    queryKey: ['artist', artistId],
    queryFn: () => api.getArtist(artistId!),
    enabled: artistId != null,
  });

  const pickBtn = 'w-full text-left rounded-lg px-2.5 py-1.5 bg-zinc-800/50 active:bg-zinc-700 transition-colors';
  const backBtn = 'text-[10px] text-zinc-500 hover:text-zinc-300 uppercase tracking-wide';

  // Step 3 — pick the track on the chosen release.
  if (artistId != null && albumId != null && artist) {
    const al = artist.albums?.find((a) => a.id === albumId);
    return (
      <div className="mt-1.5 space-y-1">
        <button onClick={() => setAlbumId(null)} className={backBtn}>← {artist.name} releases</button>
        <div className="max-h-44 overflow-y-auto space-y-0.5">
          {(al?.tracks ?? []).map((t) => (
            <button key={t.id} onClick={() => onPick(t.id)} className={pickBtn}>
              <p className="text-[11px] truncate">
                <span className="text-zinc-500 tabular-nums mr-1.5">
                  {t.disc_number > 1 ? `${t.disc_number}-` : ''}{t.track_number}
                </span>
                {t.title}
                <span className="text-zinc-600"> · {t.status}</span>
              </p>
            </button>
          ))}
        </div>
      </div>
    );
  }

  // Step 2 — pick the release under the chosen artist.
  if (artistId != null) {
    const albums = (artist?.albums ?? []).filter(
      (a) => !q || a.title.toLowerCase().includes(q.toLowerCase()),
    );
    return (
      <div className="mt-1.5 space-y-1">
        <div className="flex items-center justify-between">
          <button onClick={() => { setArtistId(null); setQ(''); }} className={backBtn}>← artists</button>
          <input
            type="text"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Filter releases…"
            className="w-40 bg-zinc-900 rounded-lg px-2.5 py-1 text-xs placeholder-zinc-600 outline-none focus:ring-2 focus:ring-zinc-600"
          />
        </div>
        <div className="max-h-44 overflow-y-auto space-y-0.5">
          {albums.map((a) => (
            <button key={a.id} onClick={() => { setAlbumId(a.id); setQ(''); }} className={pickBtn}>
              <p className="text-[11px] truncate">
                {a.title}
                <span className="text-zinc-600">{a.year ? ` · ${a.year}` : ''}{a.record_type && a.record_type !== 'album' ? ` · ${a.record_type}` : ''}</span>
              </p>
            </button>
          ))}
          {artist && albums.length === 0 && <p className="text-[11px] text-zinc-600 py-1">No releases match</p>}
        </div>
      </div>
    );
  }

  // Step 1 — pick the artist.
  const filteredArtists = (artists ?? []).filter(
    (a) => !q || a.name.toLowerCase().includes(q.toLowerCase()),
  );
  return (
    <div className="mt-1.5 space-y-1">
      <input
        type="text"
        value={q}
        onChange={(e) => setQ(e.target.value)}
        placeholder="Search artists…"
        className="w-full bg-zinc-900 rounded-lg px-3 py-1.5 text-xs placeholder-zinc-600 outline-none focus:ring-2 focus:ring-zinc-600"
        autoFocus
      />
      <div className="max-h-44 overflow-y-auto space-y-0.5">
        {filteredArtists.map((a) => (
          <button key={a.id} onClick={() => { setArtistId(a.id); setQ(''); }} className={pickBtn}>
            <p className="text-[11px] truncate">{a.name}</p>
          </button>
        ))}
        {artists && filteredArtists.length === 0 && (
          <p className="text-[11px] text-zinc-600 py-1">No artists match</p>
        )}
      </div>
    </div>
  );
}

function FileRow({
  file,
  onToggleSkip,
  onOverride,
}: {
  file: UploadFileItem;
  onToggleSkip: (id: number, skip: boolean) => void;
  onOverride: (id: number, trackId: number) => void;
}) {
  const m = file.match;
  const [overriding, setOverriding] = useState(false);
  return (
    <div className={`px-4 py-3 flex items-start gap-3 ${file.skip ? 'opacity-40' : ''}`}>
      <input
        type="checkbox"
        className="mt-1 accent-zinc-400"
        title="Include in commit"
        checked={!file.skip}
        onChange={(e) => onToggleSkip(file.id, !e.target.checked)}
      />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2">
          <span className="text-sm text-zinc-100 truncate">{file.filename}</span>
          <span className="text-xs text-zinc-500 shrink-0">{formatFileSize(file.size)}</span>
        </div>
        {file.meta && (
          <div className="text-xs text-zinc-500 truncate mt-0.5">
            {[file.meta.artist, file.meta.album, file.meta.track ? `#${file.meta.track}` : '', file.meta.title]
              .filter(Boolean)
              .join(' — ')}
            {file.meta.duration_ms > 0 && ` · ${formatDuration(file.meta.duration_ms)}`}
            {file.meta.format && ` · ${file.meta.format.toUpperCase()}`}
            {file.meta.mb_tagged && <span className="text-sky-400"> · MB</span>}
          </div>
        )}
        <div className="flex items-center gap-2 mt-1">
          {file.state === 'identified' && m && (
            <>
              <ConfidenceBadge c={m.confidence} />
              <span className="text-xs text-zinc-300 truncate">
                → {m.track_title || 'unmatched track'}
              </span>
              {m.duplicate && (
                <span className="text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded bg-orange-900/60 text-orange-300">
                  owned
                </span>
              )}
            </>
          )}
          {file.state === 'unidentified' && (
            <span className="text-xs text-red-400">{file.error || 'unidentified'}</span>
          )}
          {file.state === 'committed' && <span className="text-xs text-emerald-400">committed</span>}
          {file.state === 'skipped' && <span className="text-xs text-zinc-500">skipped</span>}
          {file.state === 'failed' && <span className="text-xs text-red-400">{file.error || 'failed'}</span>}
        </div>
        {m && <div className="text-[11px] text-zinc-600 mt-0.5">{m.reason}</div>}
        {overriding && (
          <FileOverridePicker
            onPick={(trackId) => {
              onOverride(file.id, trackId);
              setOverriding(false);
            }}
          />
        )}
      </div>
      {file.state !== 'committed' && (
        <button
          onClick={() => setOverriding(!overriding)}
          className="shrink-0 px-2 py-1 rounded text-[10px] font-medium uppercase text-zinc-500 bg-zinc-800 active:bg-zinc-700 transition-colors"
          title="Match to a different library track"
        >
          Override
        </button>
      )}
    </div>
  );
}

// Header for one identified release group: album title, provider link to the
// identified release, and the edition picker (same control as the album page —
// library albums refold in place, not-yet-imported albums pin the release the
// commit will create).
function ReleaseGroupHeader({
  batchId,
  album,
  onChanged,
}: {
  batchId: string;
  album: UploadAlbumRef;
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const isLibrary = !!album.id;
  const { data: editionsData } = useQuery<{ editions: AlbumEdition[]; current?: string }>({
    queryKey: isLibrary
      ? ['album-editions', album.id]
      : ['upload-editions', batchId, album.provider, album.provider_id],
    queryFn: () =>
      isLibrary
        ? api.getAlbumEditions(album.id!)
        : api.getUploadEditions(batchId, album.provider, album.provider_id),
    enabled: album.provider === 'musicbrainz',
    staleTime: 5 * 60_000,
    retry: false,
  });
  const setRelease = useMutation({
    mutationFn: (releaseId: string | null) =>
      isLibrary
        ? api.setAlbumEdition(album.id!, releaseId).then(() => undefined)
        : api.setUploadRelease(batchId, album.provider, album.provider_id, releaseId).then(() => undefined),
    onSuccess: onChanged,
    onError: (e: Error) => toast(e.message, 'error'),
  });
  const editions = editionsData?.editions ?? [];
  const current = isLibrary ? editionsData?.current : album.release_id;
  // Link to the specific identified release when one is known, else the
  // release-group/album page.
  const link = current
    ? (providerReleaseUrl(album.provider, current) ?? providerAlbumUrl(album.provider, album.provider_id))
    : providerAlbumUrl(album.provider, album.provider_id);

  return (
    <div className="flex items-center gap-2 px-4 py-2 bg-zinc-800/40 border-b border-zinc-800">
      <span className="text-sm font-medium text-zinc-200 truncate flex-1 min-w-0">
        {album.id ? (
          <Link to={`/album/${album.id}`} className="hover:underline">
            {album.title}
          </Link>
        ) : (
          album.title
        )}
        {album.new && (
          <span className="ml-2 text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded bg-sky-900/60 text-sky-300">
            new
          </span>
        )}
      </span>
      <ProviderBadge provider={album.provider} href={link} />
      {editions.length > 0 && (
        <select
          value={current ?? ''}
          disabled={setRelease.isPending}
          onChange={(e) => setRelease.mutate(e.target.value || null)}
          className="h-7 min-w-0 max-w-56 text-[11px] bg-zinc-800 text-zinc-400 border border-zinc-700 rounded px-1.5 disabled:opacity-50"
          title="Release edition — files re-match against this release's tracklist"
        >
          <option value="">Auto (default release)</option>
          {editions.map((e) => (
            <option key={e.id} value={e.id}>
              {[e.date || '?', e.country, e.status, e.disambiguation].filter(Boolean).join(' · ')}
              {e.track_count ? ` (${e.track_count} tracks)` : ''}
            </option>
          ))}
        </select>
      )}
    </div>
  );
}

export default function Upload() {
  const { toast } = useToast();
  const queryClient = useQueryClient();
  const [dragging, setDragging] = useState(false);
  const [batchId, setBatchId] = useState<string | null>(null);
  const [replaceDupes, setReplaceDupes] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const { data: batch } = useQuery<UploadBatch>({
    queryKey: ['upload-batch', batchId],
    queryFn: () => api.getUploadBatch(batchId!),
    enabled: !!batchId,
  });

  const { data: batches } = useQuery({
    queryKey: ['upload-batches'],
    queryFn: api.listUploadBatches,
  });

  const upload = useMutation({
    mutationFn: (files: File[]) => api.uploadFiles(files),
    onSuccess: (b) => {
      setBatchId(b.batch_id);
      queryClient.invalidateQueries({ queryKey: ['upload-batches'] });
      queryClient.setQueryData(['upload-batch', b.batch_id], b);
      toast(`Uploaded ${b.total} file(s), ${b.identified} identified`, 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const patchFile = useMutation({
    mutationFn: ({ fileId, skip, trackId }: { fileId: number; skip?: boolean; trackId?: number }) =>
      api.patchUploadFile(batchId!, fileId, { skip, track_id: trackId }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['upload-batch', batchId] }),
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const commit = useMutation({
    mutationFn: () => api.commitUpload(batchId!, replaceDupes ? 'replace' : 'skip'),
    onSuccess: (res) => {
      queryClient.invalidateQueries({ queryKey: ['artists'] });
      if (res.cleared) {
        // Fully committed batches are discarded server-side — leave the view.
        setBatchId(null);
        queryClient.invalidateQueries({ queryKey: ['upload-batches'] });
      } else {
        queryClient.invalidateQueries({ queryKey: ['upload-batch', batchId] });
      }
      toast(
        `Committed ${res.committed.length} file(s)` +
          (res.skipped.length ? `, ${res.skipped.length} skipped` : '') +
          (res.failed.length ? `, ${res.failed.length} failed` : ''),
        res.failed.length ? 'error' : 'success',
      );
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const discard = useMutation({
    mutationFn: () => api.discardUpload(batchId!),
    onSuccess: () => {
      setBatchId(null);
      queryClient.invalidateQueries({ queryKey: ['upload-batches'] });
      toast('Batch discarded', 'info');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      setDragging(false);
      const files = Array.from(e.dataTransfer.files).filter((f) =>
        ACCEPT.split(',').some((ext) => f.name.toLowerCase().endsWith(ext)),
      );
      if (files.length) upload.mutate(files);
      else toast('No supported audio files (mp3, flac, wav)', 'error');
    },
    [upload, toast],
  );

  // Group files by identified release — existing library albums group on the
  // album id, new provider albums on provider+release-group, everything else
  // falls into the unmatched group.
  const groups = useMemo(() => {
    if (!batch) return [];
    const map = new Map<string, { key: string; album?: UploadAlbumRef; files: UploadFileItem[] }>();
    const order: string[] = [];
    for (const f of batch.files) {
      const a = f.match?.album;
      const key = a ? (a.id ? `lib:${a.id}` : `new:${a.provider}:${a.provider_id}`) : 'none';
      let g = map.get(key);
      if (!g) {
        g = { key, album: a, files: [] };
        map.set(key, g);
        order.push(key);
      }
      g.files.push(f);
    }
    return order.map((k) => map.get(k)!);
  }, [batch]);

  // New-album proposals carry provider_track_id (not track_id — the row is
  // created at commit), so a file is committable if either is present.
  const commitable =
    batch?.files.filter(
      (f) =>
        f.state === 'identified' && !f.skip && (f.match?.track_id || f.match?.provider_track_id),
    ) ?? [];
  const hasDuplicates = batch?.files.some((f) => f.match?.duplicate && f.state === 'identified' && !f.skip) ?? false;
  const done = batch && batch.files.every((f) => ['committed', 'skipped', 'failed'].includes(f.state));

  return (
    <div>
      <h1 className="text-2xl font-bold mb-4">Upload</h1>

      {!batch && (
        <>
          <div
            onDragOver={(e) => {
              e.preventDefault();
              setDragging(true);
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={onDrop}
            onClick={() => inputRef.current?.click()}
            className={`border-2 border-dashed rounded-xl p-10 text-center cursor-pointer transition-colors ${
              dragging ? 'border-zinc-300 bg-zinc-800/60' : 'border-zinc-700 bg-zinc-900 hover:border-zinc-500'
            }`}
          >
            <svg className="w-10 h-10 mx-auto text-zinc-500 mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
              <polyline points="17 8 12 3 7 8" />
              <line x1="12" y1="3" x2="12" y2="15" />
            </svg>
            <p className="text-zinc-300 text-sm">Drag &amp; drop audio files here, or click to browse</p>
            <p className="text-zinc-600 text-xs mt-1">MP3, FLAC, WAV · files are identified, tagged, and filed into your library</p>
            <input
              ref={inputRef}
              type="file"
              multiple
              accept={ACCEPT}
              className="hidden"
              onChange={(e) => {
                const files = Array.from(e.target.files ?? []);
                if (files.length) upload.mutate(files);
                e.target.value = '';
              }}
            />
          </div>
          {upload.isPending && <p className="text-zinc-400 text-sm mt-3">Uploading &amp; identifying…</p>}

          {batches && batches.length > 0 && (
            <div className="mt-6">
              <h2 className="text-sm font-semibold text-zinc-400 uppercase tracking-wide mb-2">Recent batches</h2>
              <div className="divide-y divide-zinc-800 rounded-lg border border-zinc-800">
                {batches.map((b) => (
                  <button
                    key={b.batch_id}
                    onClick={() => setBatchId(b.batch_id)}
                    className="w-full text-left px-4 py-3 hover:bg-zinc-800/50 flex items-center justify-between"
                  >
                    <span className="text-sm text-zinc-200">{new Date(b.created_at).toLocaleString()}</span>
                    <span className="text-xs text-zinc-500">
                      {b.committed}/{b.total} committed · {b.unidentified} unidentified
                    </span>
                  </button>
                ))}
              </div>
            </div>
          )}
        </>
      )}

      {batch && (
        <>
          <div className="flex items-center justify-between mb-3">
            <div className="text-sm text-zinc-400" title={batch.batch_id}>
              Uploaded {new Date(batch.created_at).toLocaleString()} · {batch.identified}/{batch.total} identified
              {batch.unidentified > 0 && <span className="text-amber-400"> · {batch.unidentified} unidentified</span>}
            </div>
            <button onClick={() => setBatchId(null)} className="text-xs text-zinc-500 hover:text-zinc-300">
              ← back
            </button>
          </div>

          <div className="space-y-3">
            {groups.map((g) => (
              <div key={g.key} className="rounded-lg border border-zinc-800 overflow-hidden">
                {g.album ? (
                  <ReleaseGroupHeader
                    batchId={batch.batch_id}
                    album={g.album}
                    onChanged={() =>
                      queryClient.invalidateQueries({ queryKey: ['upload-batch', batchId] })
                    }
                  />
                ) : (
                  <div className="px-4 py-2 bg-zinc-800/40 border-b border-zinc-800 text-xs uppercase tracking-wide text-zinc-500">
                    No album match
                  </div>
                )}
                <div className="divide-y divide-zinc-800">
                  {g.files.map((f) => (
                    <FileRow
                      key={f.id}
                      file={f}
                      onToggleSkip={(id, skip) => patchFile.mutate({ fileId: id, skip })}
                      onOverride={(id, trackId) => patchFile.mutate({ fileId: id, trackId })}
                    />
                  ))}
                </div>
              </div>
            ))}
          </div>

          {!done && (
            <div className="mt-4 flex items-center gap-4">
              <button
                onClick={() => commit.mutate()}
                disabled={commitable.length === 0 || commit.isPending}
                className="px-4 py-2 rounded-lg bg-emerald-700 hover:bg-emerald-600 disabled:opacity-40 text-sm font-medium"
              >
                {commit.isPending ? 'Committing…' : `Commit ${commitable.length} file(s) to library`}
              </button>
              <button
                onClick={() => discard.mutate()}
                disabled={discard.isPending}
                className="px-4 py-2 rounded-lg bg-zinc-800 hover:bg-zinc-700 text-sm text-zinc-300"
              >
                Discard
              </button>
              {hasDuplicates && (
                <label className="flex items-center gap-2 text-xs text-zinc-400">
                  <input
                    type="checkbox"
                    className="accent-zinc-400"
                    checked={replaceDupes}
                    onChange={(e) => setReplaceDupes(e.target.checked)}
                  />
                  Replace already-owned files
                </label>
              )}
            </div>
          )}

          {done && (
            <button
              onClick={() => {
                discard.mutate();
              }}
              className="mt-4 px-4 py-2 rounded-lg bg-zinc-800 hover:bg-zinc-700 text-sm text-zinc-300"
            >
              Done — clear batch
            </button>
          )}
        </>
      )}
    </div>
  );
}
