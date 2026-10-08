import type { Artist, Album, AlbumEdition, Track, SearchResponse, BrowseArtistResult, BrowseAlbumDetail, DownloadQueueItem, DownloadProgress, SystemStatus, ProviderInfo, ActivityResponse, ManualSearchStart, ManualSearchResponse, LibrarySearchResult, TrackSearchResult, BlacklistEntry, UserCooldown, ImportState, UploadBatch, UploadBatchSummary, UploadCommitResult, ArtistSyncStatus } from '../types/index';

const BASE = '/api';

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(body.error || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

export const api = {
  search: (q: string, provider?: string, limit = 25, offset = 0) =>
    request<SearchResponse>(`/search?q=${encodeURIComponent(q)}&limit=${limit}&offset=${offset}${provider ? `&provider=${encodeURIComponent(provider)}` : ''}`),

  browseArtist: (id: string, provider?: string) =>
    request<BrowseArtistResult>(`/browse/artist/${id}${provider ? `?provider=${encodeURIComponent(provider)}` : ''}`),
  browseAlbum: (id: string, provider?: string) =>
    request<BrowseAlbumDetail>(`/browse/album/${id}${provider ? `?provider=${encodeURIComponent(provider)}` : ''}`),

  watchArtist: (id: string, opts?: { watch_new_releases?: boolean; provider?: string }) =>
    request<Artist>(`/watch/artist/${id}`, {
      method: 'POST',
      body: JSON.stringify(opts ?? {}),
    }),
  watchAlbum: (id: string, artistInfo: { artist_provider_id: string; artist_name: string; artist_image_url: string; provider?: string }) =>
    request<Album>(`/watch/album/${id}`, {
      method: 'POST',
      body: JSON.stringify(artistInfo),
    }),
  watchTrack: (id: string, trackInfo: Record<string, unknown>) =>
    request<Track>(`/watch/track/${id}`, {
      method: 'POST',
      body: JSON.stringify(trackInfo),
    }),

  listArtists: () => request<Artist[]>('/artists'),
  getArtist: (id: number) => request<Artist>(`/artists/${id}`),
  getAlbum: (id: number) => request<Album>(`/albums/${id}`),

  queueTrack: (id: number) =>
    request<void>(`/tracks/${id}/queue`, { method: 'POST' }),
  startManualSearch: (id: number, query?: string) =>
    request<ManualSearchStart>(`/tracks/${id}/search`, {
      method: 'POST',
      body: query ? JSON.stringify({ query }) : undefined,
    }),
  pollManualSearch: (trackId: number, searchId: string) =>
    request<ManualSearchResponse>(`/tracks/${trackId}/search/${searchId}`),
  deleteManualSearch: (trackId: number, searchId: string) =>
    request<void>(`/tracks/${trackId}/search/${searchId}`, { method: 'DELETE' }),
  manualDownloadTrack: (id: number, username: string, filename: string, size: number, bitRate: number) =>
    request<{ status: string }>(`/tracks/${id}/download`, {
      method: 'POST',
      body: JSON.stringify({ username, filename, size, bit_rate: bitRate }),
    }),
  queueArtistTracks: (id: number) =>
    request<{ queued: number }>(`/artists/${id}/queue`, { method: 'POST' }),
  queueAlbumTracks: (id: number) =>
    request<{ queued: number }>(`/albums/${id}/queue`, { method: 'POST' }),

  toggleNewReleases: (id: number, enabled: boolean) =>
    request<{ watch_new_releases: boolean }>(`/artists/${id}/new-releases`, {
      method: 'PUT',
      body: JSON.stringify({ enabled }),
    }),
  refreshArtist: (id: number) =>
    request<{ status: string; reconciling: boolean }>(`/artists/${id}/refresh`, { method: 'POST' }),
  setArtistReleaseTypes: (id: number, types: Record<string, boolean> | null) =>
    request<void>(`/artists/${id}/release-types`, {
      method: 'PUT',
      body: JSON.stringify({ types }),
    }),
  addArtistRelease: (id: number, url: string) =>
    request<{ album_id: number; existed?: boolean }>(`/artists/${id}/releases`, {
      method: 'POST',
      body: JSON.stringify({ url }),
    }),
  getUpcomingReleases: () => request<Album[]>('/releases/upcoming'),
  refreshArtists: (ids: number[]) =>
    request<{ queued: number; ids: number[] }>(`/artists/refresh`, { method: 'POST', body: JSON.stringify({ ids }) }),
  getSyncStatus: () => request<{ items: ArtistSyncStatus[] }>('/artists/sync'),
  setArtistsNewReleases: (ids: number[], enabled: boolean) =>
    request<{ updated: number }>(`/artists/new-releases`, {
      method: 'PUT',
      body: JSON.stringify({ ids, enabled }),
    }),

  unwatchArtist: (id: number) =>
    request<void>(`/artists/${id}`, { method: 'DELETE' }),
  unwatchAlbum: (id: number) =>
    request<void>(`/albums/${id}`, { method: 'DELETE' }),
  unwatchTrack: (id: number) =>
    request<void>(`/tracks/${id}`, { method: 'DELETE' }),

  refreshAlbum: (id: number) =>
    request<{ ok: boolean }>(`/albums/${id}/refresh`, { method: 'POST' }),
  getAlbumEditions: (id: number) =>
    request<{ editions: AlbumEdition[]; current?: string }>(`/albums/${id}/editions`),
  setAlbumEdition: (id: number, releaseId: string | null) =>
    request<{ release_id?: string; matched: number; added: number; pruned: number }>(`/albums/${id}/edition`, {
      method: 'PUT',
      body: JSON.stringify({ release_id: releaseId }),
    }),
  splitAlbumTracks: (id: number, releaseId: string, trackIds: number[], url?: string) =>
    request<{ album_id: number; moved: number; relocated: number; added: number; restored: number }>(`/albums/${id}/split-tracks`, {
      method: 'POST',
      body: JSON.stringify({ release_id: releaseId, url, track_ids: trackIds }),
    }),
  ignoreAlbum: (id: number) =>
    request<void>(`/albums/${id}/ignore`, { method: 'PUT' }),
  unignoreAlbum: (id: number) =>
    request<void>(`/albums/${id}/ignore`, { method: 'DELETE' }),
  ignoreTrack: (id: number) =>
    request<void>(`/tracks/${id}/ignore`, { method: 'PUT' }),
  unignoreTrack: (id: number) =>
    request<void>(`/tracks/${id}/ignore`, { method: 'DELETE' }),

  listDownloads: (status?: string) =>
    request<DownloadQueueItem[]>(`/downloads${status ? `?status=${status}` : ''}`),
  getDownloadProgress: () =>
    request<DownloadProgress[]>('/downloads/progress'),
  queueDownloads: () =>
    request<{ queued: number }>('/downloads/queue', { method: 'POST' }),
  retryDownload: (id: number) =>
    request<void>(`/downloads/${id}/retry`, { method: 'POST' }),
  deleteDownload: (id: number) =>
    request<void>(`/downloads/${id}`, { method: 'DELETE' }),
  clearDownloadsByStatus: (status: string) =>
    request<{ deleted: number }>(`/downloads/clear?status=${status}`, { method: 'DELETE' }),

  getSettings: () => request<Record<string, string>>('/settings'),
  updateSettings: (settings: Record<string, string>) =>
    request<Record<string, string>>('/settings', {
      method: 'PUT',
      body: JSON.stringify(settings),
    }),
  namingPreview: (template: string) =>
    request<{ path: string }>(`/settings/naming-preview?template=${encodeURIComponent(template)}`),
  startLibraryImport: (dryRun: boolean) =>
    request<ImportState>('/library/import', {
      method: 'POST',
      body: JSON.stringify({ dry_run: dryRun }),
    }),
  libraryImportStatus: () => request<ImportState>('/library/import'),

  getStatus: () => request<SystemStatus>('/status'),

  listProviders: () => request<ProviderInfo[]>('/providers'),
  listActivity: (limit = 50, offset = 0, action = '') =>
    request<ActivityResponse>(`/activity?limit=${limit}&offset=${offset}${action ? `&action=${encodeURIComponent(action)}` : ''}`),
  clearCache: () => request<void>('/cache', { method: 'DELETE' }),
  relinkEntity: (type: 'artist' | 'album' | 'track', id: number, providerID: string) =>
    request<{ status: string; reconciling?: boolean }>(`/relink/${type}/${id}`, {
      method: 'POST',
      body: JSON.stringify({ provider_id: providerID }),
    }),

  // Manual link: merge a leftover local album into a linked sibling album, or
  // claim a wanted track with an owned local file. See ADR-0007.
  linkAlbum: (id: number, targetAlbumID: number) =>
    request<{ status: string; merged_into: number }>(`/albums/${id}/link`, {
      method: 'POST',
      body: JSON.stringify({ target_album_id: targetAlbumID }),
    }),
  linkTrack: (id: number, targetTrackID: number) =>
    request<{ status: string; claimed: number }>(`/tracks/${id}/link`, {
      method: 'POST',
      body: JSON.stringify({ target_track_id: targetTrackID }),
    }),

  searchLibrary: (q: string) =>
    request<LibrarySearchResult[]>(`/library/search?q=${encodeURIComponent(q)}`),
  searchBrowseArtistTracks: (id: string, q: string, provider?: string) =>
    request<{ tracks: TrackSearchResult[] }>(`/browse/artist/${id}/tracks?q=${encodeURIComponent(q)}${provider ? `&provider=${encodeURIComponent(provider)}` : ''}`),

  listBlacklist: () => request<BlacklistEntry[]>('/blacklist'),
  deleteBlacklistEntry: (id: number) =>
    request<void>(`/blacklist/${id}`, { method: 'DELETE' }),
  clearBlacklist: () => request<void>('/blacklist', { method: 'DELETE' }),

  listCooldowns: () => request<UserCooldown[]>('/cooldowns'),
  deleteCooldown: (id: number) =>
    request<void>(`/cooldowns/${id}`, { method: 'DELETE' }),
  clearCooldowns: () => request<void>('/cooldowns', { method: 'DELETE' }),

  // Uploads — multipart, so no JSON Content-Type header.
  uploadFiles: async (files: File[]) => {
    const fd = new FormData();
    for (const f of files) fd.append('files', f, f.name);
    const res = await fetch(`${BASE}/uploads`, { method: 'POST', body: fd });
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new Error(body.error || res.statusText);
    }
    return res.json() as Promise<UploadBatch>;
  },
  listUploadBatches: () => request<UploadBatchSummary[]>('/uploads'),
  getUploadBatch: (batch: string) => request<UploadBatch>(`/uploads/${batch}`),
  identifyUpload: (batch: string) =>
    request<UploadBatch>(`/uploads/${batch}/identify`, { method: 'POST' }),
  getUploadEditions: (batch: string, provider: string, id: string) =>
    request<{ editions: AlbumEdition[] }>(`/uploads/${batch}/editions?provider=${encodeURIComponent(provider)}&id=${encodeURIComponent(id)}`),
  setUploadRelease: (batch: string, provider: string, providerId: string, releaseId: string | null) =>
    request<UploadBatch>(`/uploads/${batch}/release`, {
      method: 'PUT',
      body: JSON.stringify({ provider, provider_id: providerId, release_id: releaseId }),
    }),
  patchUploadFile: (batch: string, fileId: number, body: { track_id?: number; skip?: boolean }) =>
    request<{ status: string }>(`/uploads/${batch}/files/${fileId}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),
  commitUpload: (batch: string, onDuplicate: 'skip' | 'replace') =>
    request<UploadCommitResult>(`/uploads/${batch}/commit`, {
      method: 'POST',
      body: JSON.stringify({ on_duplicate: onDuplicate }),
    }),
  discardUpload: (batch: string) =>
    request<void>(`/uploads/${batch}`, { method: 'DELETE' }),
};
