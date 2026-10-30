import { useCallback, useEffect, useState } from 'react';
import { GatewayError, SessionRequest, closeSession, createSession, getSession } from './backup-browse';

/** The time between two checks of a starting session. Every request is short; none waits for the helper. */
export const POLL_MS = 2000;

export type SessionStatus = 'starting' | 'ready' | 'ended' | 'error';

export interface BrowseSessionState {
  status: SessionStatus;
  /** The name of the session, once there is one. */
  session?: string;
  /** What the helper says about its start, or why the session ended. */
  message?: string;
  error?: GatewayError;
  /** True when the session reads the file systems inside a VM disk. */
  guest?: boolean;
  /** Opens a new session, after one ended. */
  reopen: () => void;
}

/**
 * Opens a browse session for a backup, waits for it by polling, and closes it when the page is left, the
 * backup or the moment changes, or the browser tab is closed. A session holds a helper pod, so it is never
 * left open on purpose; the driver also closes an idle one.
 */
export function useBrowseSession(req: SessionRequest | undefined): BrowseSessionState {
  const [state, setState] = useState<Omit<BrowseSessionState, 'reopen'>>({ status: 'starting' });
  const [attempt, setAttempt] = useState(0);
  const reopen = useCallback(() => setAttempt((n) => n + 1), []);
  const { policy, namespace, pvc, moment } = req || { policy: '', namespace: '', pvc: '', moment: undefined };
  const ready = !!req;

  useEffect(() => {
    if (!ready) return undefined;
    let cancelled = false;
    let name: string | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let closed = false;

    const close = (keepalive = false) => {
      if (!name || (closed && !keepalive)) return;
      closed = true;
      (keepalive ? closeSession(name, true) : closeSession(name)).catch(() => undefined);
    };

    const apply = (s: { name: string; phase: string; message?: string; guest?: boolean }): boolean => {
      const guest = !!s.guest;
      switch (s.phase) {
        case 'Ready':
          setState({ status: 'ready', session: s.name, guest });
          return true;
        case 'Closed':
        case 'Failed':
          setState({ status: 'ended', session: s.name, message: s.message, guest });
          return true;
        default:
          setState({ status: 'starting', session: s.name, message: s.message, guest });
          return false;
      }
    };

    const poll = async () => {
      if (cancelled || !name) return;
      try {
        const s = await getSession(name);
        if (cancelled) return;
        if (!apply(s)) timer = setTimeout(poll, POLL_MS);
      } catch (e) {
        if (cancelled) return;
        const err = e as GatewayError;
        if (err.status === 410 || err.status === 404) {
          setState({ status: 'ended', session: name, message: err.message });
        } else {
          setState({ status: 'error', session: name, error: err });
        }
      }
    };

    const start = async () => {
      setState({ status: 'starting' });
      try {
        const s = await createSession({ policy, namespace, pvc, ...(moment ? { moment } : {}) });
        name = s.name;
        if (cancelled) {
          close();
          return;
        }
        if (!apply(s)) timer = setTimeout(poll, POLL_MS);
      } catch (e) {
        if (!cancelled) setState({ status: 'error', error: e as GatewayError });
      }
    };

    const onPageHide = () => close(true);
    window.addEventListener('pagehide', onPageHide);
    start();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
      window.removeEventListener('pagehide', onPageHide);
      close();
    };
  }, [ready, policy, namespace, pvc, moment, attempt]);

  return { ...state, reopen };
}
