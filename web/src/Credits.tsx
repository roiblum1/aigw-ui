import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import BrandMark from "./BrandMark";

/** How many clicks on the logo, each within this long of the first, open the credits. */
const CLICKS = 5;
const WITHIN_MS = 1500;
/** The credits close by themselves after this long. */
const SHOWN_MS = 9000;

/**
 * The logo, which opens the credits when it is clicked five times in a row,
 * quickly. Wrap the logo with it.
 */
export function CreditsTrigger(props: { children: ReactNode; className?: string }) {
  const [open, setOpen] = useState(false);
  const clicks = useRef<number[]>([]);
  const close = useCallback(() => setOpen(false), []);
  const click = () => {
    const now = Date.now();
    clicks.current = [...clicks.current.filter((t) => now - t < WITHIN_MS), now];
    if (clicks.current.length >= CLICKS) {
      clicks.current = [];
      setOpen(true);
    }
  };
  return (
    <>
      <span className={`credits-trigger ${props.className ?? ""}`} onClick={click}>
        {props.children}
      </span>
      {open && <Credits onClose={close} />}
    </>
  );
}

function Credits(props: { onClose: () => void }) {
  const { onClose } = props;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    const timer = setTimeout(onClose, SHOWN_MS);
    return () => {
      window.removeEventListener("keydown", onKey);
      clearTimeout(timer);
    };
  }, [onClose]);
  return (
    <div className="credits" role="dialog" aria-modal="true" aria-label="Credits" onClick={onClose}>
      <div className="credits-card">
        {Array.from({ length: 12 }, (_, i) => (
          <span
            key={i}
            className="credits-spark"
            style={{ top: `${8 + ((i * 37) % 84)}%`, left: `${6 + ((i * 53) % 88)}%`, animationDelay: `${i * 0.2}s` }}
            aria-hidden
          />
        ))}
        <BrandMark className="credits-mark" />
        <p className="credits-kicker">Invented, designed and implemented by</p>
        <p className="credits-pre">the one and only</p>
        <p className="credits-name">Roi Blum</p>
        <p className="credits-hint">Click anywhere or press Esc to close</p>
      </div>
    </div>
  );
}

/** The line at the end of every page. */
export function Footer(props: { className?: string }) {
  return <footer className={`app-footer ${props.className ?? ""}`}>© Roi Blum</footer>;
}
