import { act, renderHook } from '@testing-library/react';
import { GatewayError, SessionRequest, closeSession, createSession, getSession } from './backup-browse';
import { POLL_MS, useBrowseSession } from './use-browse-session';

jest.mock('./backup-browse', () => ({
  ...jest.requireActual('./backup-browse'),
  createSession: jest.fn(),
  getSession: jest.fn(),
  closeSession: jest.fn(),
}));

const req = { policy: 'nightly', namespace: 'apps', pvc: 'db' };
const flush = async () => {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
};

beforeEach(() => {
  jest.useFakeTimers();
  jest.clearAllMocks();
  (closeSession as jest.Mock).mockResolvedValue({});
});
afterEach(() => jest.useRealTimers());

describe('useBrowseSession', () => {
  it('starts, polls with short requests, and becomes ready', async () => {
    (createSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Pending' });
    (getSession as jest.Mock)
      .mockResolvedValueOnce({ name: 'bb-1', phase: 'Starting', message: 'starting the helper' })
      .mockResolvedValueOnce({ name: 'bb-1', phase: 'Ready' });
    const { result } = renderHook(() => useBrowseSession(req));
    expect(result.current.status).toBe('starting');
    await flush();
    expect(result.current.status).toBe('starting');
    await act(async () => {
      jest.advanceTimersByTime(POLL_MS);
    });
    await flush();
    expect(result.current.message).toBe('starting the helper');
    await act(async () => {
      jest.advanceTimersByTime(POLL_MS);
    });
    await flush();
    expect(result.current.status).toBe('ready');
    expect(result.current.session).toBe('bb-1');
    expect(createSession).toHaveBeenCalledTimes(1);
  });

  it('is ready at once when the session is already ready', async () => {
    (createSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Ready' });
    const { result } = renderHook(() => useBrowseSession(req));
    await flush();
    expect(result.current.status).toBe('ready');
    expect(getSession).not.toHaveBeenCalled();
  });

  it('shows a failed session as ended, with the reason', async () => {
    (createSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Failed', message: 'the volume is gone' });
    const { result } = renderHook(() => useBrowseSession(req));
    await flush();
    expect(result.current.status).toBe('ended');
    expect(result.current.message).toBe('the volume is gone');
  });

  it('shows an ended session when the gateway says 410', async () => {
    (createSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Pending' });
    (getSession as jest.Mock).mockRejectedValue(new GatewayError(410, 'session_ended', 'gone'));
    const { result } = renderHook(() => useBrowseSession(req));
    await flush();
    await act(async () => {
      jest.advanceTimersByTime(POLL_MS);
    });
    await flush();
    expect(result.current.status).toBe('ended');
  });

  it('shows an error when the session cannot be made', async () => {
    (createSession as jest.Mock).mockRejectedValue(new GatewayError(403, 'forbidden', 'no'));
    const { result } = renderHook(() => useBrowseSession(req));
    await flush();
    expect(result.current.status).toBe('error');
    expect(result.current.error?.status).toBe(403);
  });

  it('closes the session when the page is left, and stops polling', async () => {
    (createSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Pending' });
    (getSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Pending' });
    const { unmount } = renderHook(() => useBrowseSession(req));
    await flush();
    unmount();
    expect(closeSession).toHaveBeenCalledWith('bb-1');
    await act(async () => {
      jest.advanceTimersByTime(POLL_MS * 5);
    });
    expect(getSession).not.toHaveBeenCalled();
  });

  it('closes a session that was made after the page was left', async () => {
    let resolve: (v: unknown) => void = () => undefined;
    (createSession as jest.Mock).mockReturnValue(new Promise((r) => (resolve = r)));
    const { unmount } = renderHook(() => useBrowseSession(req));
    unmount();
    expect(closeSession).not.toHaveBeenCalled();
    resolve({ name: 'bb-9', phase: 'Pending' });
    await flush();
    expect(closeSession).toHaveBeenCalledWith('bb-9');
    expect(getSession).not.toHaveBeenCalled();
  });

  it('closes the old session and opens a new one when the moment changes', async () => {
    (createSession as jest.Mock)
      .mockResolvedValueOnce({ name: 'bb-1', phase: 'Ready' })
      .mockResolvedValueOnce({ name: 'bb-2', phase: 'Ready' });
    const { result, rerender } = renderHook((r: SessionRequest) => useBrowseSession(r), { initialProps: req as SessionRequest });
    await flush();
    expect(result.current.session).toBe('bb-1');
    rerender({ ...req, moment: '2026-10-22T09:15:00Z' });
    await flush();
    expect(closeSession).toHaveBeenCalledWith('bb-1');
    expect(result.current.session).toBe('bb-2');
    expect((createSession as jest.Mock).mock.calls[1][0].moment).toBe('2026-10-22T09:15:00Z');
  });

  it('opens a new session when asked again after the old one ended', async () => {
    (createSession as jest.Mock)
      .mockResolvedValueOnce({ name: 'bb-1', phase: 'Closed' })
      .mockResolvedValueOnce({ name: 'bb-2', phase: 'Ready' });
    const { result } = renderHook(() => useBrowseSession(req));
    await flush();
    expect(result.current.status).toBe('ended');
    act(() => result.current.reopen());
    await flush();
    expect(result.current.status).toBe('ready');
    expect(result.current.session).toBe('bb-2');
  });

  it('keeps the close request alive when the browser tab is closed', async () => {
    (createSession as jest.Mock).mockResolvedValue({ name: 'bb-1', phase: 'Ready' });
    renderHook(() => useBrowseSession(req));
    await flush();
    window.dispatchEvent(new Event('pagehide'));
    expect(closeSession).toHaveBeenCalledWith('bb-1', true);
  });
});
