export function formatDuration(ms: number): string {
  const mins = Math.floor(ms / 60000);
  const secs = Math.floor((ms % 60000) / 1000);
  return `${mins}:${secs.toString().padStart(2, '0')}`;
}

export function formatFans(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}K`;
  return n.toString();
}

export function formatTotalDuration(ms: number): string {
  const hours = Math.floor(ms / 3600000);
  const mins = Math.floor((ms % 3600000) / 60000);
  if (hours > 0) return `${hours}h ${mins}m`;
  return `${mins}m`;
}

export function formatFileSize(bytes: number): string {
  if (bytes >= 1_000_000_000) return `${(bytes / 1_000_000_000).toFixed(1)} GB`;
  if (bytes >= 1_000_000) return `${(bytes / 1_000_000).toFixed(1)} MB`;
  if (bytes >= 1_000) return `${(bytes / 1_000).toFixed(0)} KB`;
  return `${bytes} B`;
}

export function formatSpeed(bps: number): string {
  if (bps >= 1_000_000) return `${(bps / 1_000_000).toFixed(1)} MB/s`;
  if (bps >= 1_000) return `${(bps / 1_000).toFixed(0)} KB/s`;
  return `${bps.toFixed(0)} B/s`;
}

export function formatRelativeDate(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  const days = Math.floor((Date.now() - d.getTime()) / 86400000);
  if (days <= 0) return 'today';
  if (days === 1) return 'yesterday';
  if (days < 30) return `${days}d ago`;
  if (days < 365) return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

const PROVIDER_ARTIST_URL: Record<string, (id: string) => string> = {
  musicbrainz: (id) => `https://musicbrainz.org/artist/${id}`,
  deezer: (id) => `https://www.deezer.com/artist/${id}`,
};

export const PROVIDER_LABEL: Record<string, string> = {
  musicbrainz: 'MusicBrainz',
  deezer: 'Deezer',
};

export function providerArtistUrl(provider: string, providerId: string): string | undefined {
  return PROVIDER_ARTIST_URL[provider]?.(providerId);
}

const PROVIDER_ALBUM_URL: Record<string, (id: string) => string> = {
  // Album provider ids are release-group ids in the MusicBrainz namespace.
  musicbrainz: (id) => `https://musicbrainz.org/release-group/${id}`,
  deezer: (id) => `https://www.deezer.com/album/${id}`,
};

export function providerAlbumUrl(provider: string, providerId: string): string | undefined {
  return PROVIDER_ALBUM_URL[provider]?.(providerId);
}

// Release-level (edition) URLs — distinct from the release-group album URLs.
const PROVIDER_RELEASE_URL: Record<string, (id: string) => string> = {
  musicbrainz: (id) => `https://musicbrainz.org/release/${id}`,
};

export function providerReleaseUrl(provider: string, id: string): string | undefined {
  return PROVIDER_RELEASE_URL[provider]?.(id);
}

const RECORD_TYPE_LABELS: Record<string, string> = {
  album: 'Album',
  ep: 'EP',
  single: 'Single',
  compilation: 'Compilation',
  live: 'Live',
  remix: 'Remix',
  soundtrack: 'Soundtrack',
  'dj-mix': 'DJ Mix',
  mixtape: 'Mixtape',
  demo: 'Demo',
  spokenword: 'Spoken Word',
  interview: 'Interview',
  audiobook: 'Audiobook',
  'field-recording': 'Field Recording',
  'audio-drama': 'Audio Drama',
};

export function recordTypeLabel(recordType: string): string {
  const t = recordType || 'album';
  return RECORD_TYPE_LABELS[t] ?? t.charAt(0).toUpperCase() + t.slice(1);
}
