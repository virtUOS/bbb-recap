import { useEffect, useId, useRef } from "react";
import { createPortal } from "react-dom";

interface ConfirmDialogProps {
  title: string;
  text: string;
  confirmLabel: string;
  cancelLabel: string;
  onConfirm: () => void;
  onClose: () => void;
}

const styles = {
  dialog: {
    maxWidth: "28rem",
    padding: "1.5rem",
    border: "none",
    borderRadius: "0.5rem",
    boxShadow: "0 0.5rem 2rem rgba(0, 0, 0, 0.3)",
    fontFamily: "inherit",
    color: "#222",
    background: "#fff",
  },
  title: { margin: "0 0 0.75rem", fontSize: "1.25rem" },
  text: { margin: "0 0 1.5rem", lineHeight: 1.5 },
  buttons: { display: "flex", justifyContent: "flex-end", gap: "0.75rem" },
  button: {
    padding: "0.5rem 1rem",
    borderRadius: "0.25rem",
    border: "1px solid #888",
    background: "#fff",
    color: "#222",
    font: "inherit",
    cursor: "pointer",
  },
  confirm: { background: "#0f70d7", borderColor: "#0f70d7", color: "#fff" },
} as const;

// A modal confirmation like the one of BBB's record button. A native <dialog> on document.body: the plugin's own
// container sits behind the client (z-index -1), and showModal() puts the dialog on top, traps the focus and closes
// on Escape.
export default function ConfirmDialog({
  title,
  text,
  confirmLabel,
  cancelLabel,
  onConfirm,
  onClose,
}: ConfirmDialogProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const dialog = ref.current;
    dialog?.showModal();
    return () => dialog?.close();
  }, []);

  return createPortal(
    <dialog ref={ref} aria-labelledby={titleId} style={styles.dialog} onClose={onClose}>
      <h2 id={titleId} style={styles.title}>
        {title}
      </h2>
      <p style={styles.text}>{text}</p>
      <div style={styles.buttons}>
        <button type="button" style={styles.button} onClick={() => ref.current?.close()}>
          {cancelLabel}
        </button>
        <button
          type="button"
          style={{ ...styles.button, ...styles.confirm }}
          onClick={() => {
            onConfirm();
            ref.current?.close();
          }}
        >
          {confirmLabel}
        </button>
      </div>
    </dialog>,
    document.body,
  );
}
