type Media = Pick<HTMLVideoElement, "playbackRate" | "paused" | "play" | "pause">;
type Session = { media: Media; rate: number; wasPlaying: boolean };

/** Temporary forward playback only: never seek or change the saved default rate. */
export function createPlaybackAcceleration() {
  let session: Session | null = null;
  const stop = () => {
    const previous = session;
    session = null;
    if (previous) {
      try { previous.media.playbackRate = previous.rate; } catch { /* Media may have been detached. */ }
      if (!previous.wasPlaying) previous.media.pause();
    }
    return previous;
  };
  const start = (media: Media, multiplier: number, onError: () => void): number | null => {
    if (session && session.media !== media) stop();
    const fresh = !session;
    const current = session ?? { media, rate: media.playbackRate, wasPlaying: !media.paused };
    session = current;
    try {
      media.playbackRate = Math.max(current.rate, Math.min(8, current.rate * multiplier));
      if (fresh && media.paused) {
        void media.play().catch(() => {
          if (session !== current) return;
          stop();
          onError();
        });
      }
      return media.playbackRate;
    } catch {
      stop();
      onError();
      return null;
    }
  };
  return { start, stop };
}
