import { useCallback, useRef, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { formatFileSize, formatDuration } from '../lib/format';
import { useToast } from '../components/Toast';
import type { UploadBatch, UploadFileItem } from '../types/index';

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

function FileRow({
  file,
  onToggleSkip,
}: {
  file: UploadFileItem;
  onToggleSkip: (id: number, skip: boolean) => void;
}) {
  const m = file.match;
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
                {m.album && ` (${m.album.title}${m.album.new ? ' — new album' : ''})`}
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
      </div>
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
    mutationFn: ({ fileId, skip }: { fileId: number; skip: boolean }) =>
      api.patchUploadFile(batchId!, fileId, { skip }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['upload-batch', batchId] }),
  });

  const commit = useMutation({
    mutationFn: () => api.commitUpload(batchId!, replaceDupes ? 'replace' : 'skip'),
    onSuccess: (res) => {
      queryClient.invalidateQueries({ queryKey: ['upload-batch', batchId] });
      queryClient.invalidateQueries({ queryKey: ['artists'] });
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
                    <span className="text-sm text-zinc-200 font-mono">{b.batch_id}</span>
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
            <div className="text-sm text-zinc-400">
              <span className="font-mono">{batch.batch_id}</span> · {batch.identified}/{batch.total} identified
              {batch.unidentified > 0 && <span className="text-amber-400"> · {batch.unidentified} unidentified</span>}
            </div>
            <button onClick={() => setBatchId(null)} className="text-xs text-zinc-500 hover:text-zinc-300">
              ← back
            </button>
          </div>

          <div className="divide-y divide-zinc-800 rounded-lg border border-zinc-800">
            {batch.files.map((f) => (
              <FileRow
                key={f.id}
                file={f}
                onToggleSkip={(id, skip) => patchFile.mutate({ fileId: id, skip })}
              />
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
