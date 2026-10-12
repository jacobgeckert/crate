import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';

// AuthGate renders the login screen whenever the server requires auth and the
// session isn't valid. With no credentials configured server-side it passes
// straight through — zero cost on open LAN installs.
export default function AuthGate({ children }: { children: React.ReactNode }) {
  const queryClient = useQueryClient();
  const [expired, setExpired] = useState(false);
  const { data: me, isLoading } = useQuery({
    queryKey: ['auth-me'],
    queryFn: api.authMe,
    staleTime: Infinity,
    retry: false,
  });

  useEffect(() => {
    const onExpired = () => setExpired(true);
    window.addEventListener('crate:auth-expired', onExpired);
    return () => window.removeEventListener('crate:auth-expired', onExpired);
  }, []);

  if (isLoading) {
    return <div className="min-h-dvh bg-zinc-950" />;
  }
  if (me?.enabled && (!me.authenticated || expired)) {
    return <Login onSuccess={() => {
      setExpired(false);
      queryClient.invalidateQueries({ queryKey: ['auth-me'] });
      queryClient.invalidateQueries(); // session data may have changed hands
    }} />;
  }
  return <>{children}</>;
}

function Login({ onSuccess }: { onSuccess: () => void }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const login = useMutation({
    mutationFn: () => api.login(username, password),
    onSuccess,
    onError: (err: Error) => setError(err.message),
  });

  return (
    <div className="min-h-dvh bg-zinc-950 text-zinc-100 flex items-center justify-center px-4">
      <form
        onSubmit={(e) => { e.preventDefault(); setError(''); login.mutate(); }}
        className="w-full max-w-xs bg-zinc-900 border border-zinc-800 rounded-2xl p-6 space-y-4"
      >
        <div className="flex items-center gap-3 justify-center">
          <img src="/cratelogo.png" alt="Crate" className="w-10 h-10" />
          <h1 className="text-xl font-bold tracking-tight">Crate</h1>
        </div>
        <input
          type="text"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          placeholder="Username"
          autoComplete="username"
          autoFocus
          className="w-full bg-zinc-800 border border-zinc-700 rounded-lg px-3 py-2.5 text-sm placeholder-zinc-500 focus:outline-none focus:border-zinc-500"
        />
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          placeholder="Password"
          autoComplete="current-password"
          className="w-full bg-zinc-800 border border-zinc-700 rounded-lg px-3 py-2.5 text-sm placeholder-zinc-500 focus:outline-none focus:border-zinc-500"
        />
        {error && <p className="text-xs text-red-400">{error}</p>}
        <button
          type="submit"
          disabled={login.isPending || !username || !password}
          className="w-full bg-blue-600 hover:bg-blue-500 disabled:opacity-40 rounded-lg py-2.5 text-sm font-medium transition-colors"
        >
          {login.isPending ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </div>
  );
}
