// The popovers of the pill: start (from off) and pause (from running). Resuming needs none. The text says what is
// recorded, not where the minutes go.
import type { ReactNode } from "react";

import { chatIcon, eyeIcon, micIcon, pauseFilledIcon, sparkleFilledIcon, sparkleOutlineIcon } from "./icons";
import type { Messages } from "./messages";
import { PILL_DATA_TEST } from "./nav-items";
import Popover, { buttonStyles, closeDialogOf, colors } from "./popover";

interface Props {
  messages: Messages;
  onConfirm: () => void;
  onClose: () => void;
}

function Row({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return (
    <div style={{ display: "flex", gap: 10 }}>
      {icon}
      <span>{children}</span>
    </div>
  );
}

function Actions({ messages, confirm, icon, onConfirm }: Props & { confirm: string; icon: ReactNode }) {
  return (
    <>
      <button type="button" style={buttonStyles.text} onClick={(e) => closeDialogOf(e.currentTarget)}>
        {messages.cancel}
      </button>
      <button
        type="button"
        style={buttonStyles.primary}
        onClick={(e) => {
          onConfirm();
          closeDialogOf(e.currentTarget);
        }}
      >
        {icon}
        {confirm}
      </button>
    </>
  );
}

export function StartPopover(props: Props) {
  const { messages, onClose } = props;
  return (
    <Popover
      anchor={PILL_DATA_TEST}
      width={400}
      padding="22px 22px 18px"
      title={messages.startTitle}
      titleStyle={{ fontSize: 18, fontWeight: 700 }}
      titleIcon={sparkleOutlineIcon(22)}
      actions={<Actions {...props} confirm={messages.startButton} icon={sparkleFilledIcon(18, "#fff")} />}
      onClose={onClose}
    >
      <div
        style={{ display: "flex", flexDirection: "column", gap: 10, fontSize: 15, lineHeight: 1.4, color: colors.body }}
      >
        <Row icon={micIcon()}>
          <b style={{ color: colors.text }}>{messages.startSpoken}</b>
          {messages.startSpokenDetail} {messages.noVideo}
        </Row>
        <Row icon={chatIcon()}>{messages.startData}</Row>
        <Row icon={eyeIcon()}>{messages.startNotice}</Row>
      </div>
    </Popover>
  );
}

export function PausePopover(props: Props) {
  const { messages, onClose } = props;
  return (
    <Popover
      anchor={PILL_DATA_TEST}
      width={380}
      padding="22px 22px 18px"
      title={messages.pauseTitle}
      titleStyle={{ fontSize: 18, fontWeight: 700 }}
      actions={<Actions {...props} confirm={messages.pauseButton} icon={pauseFilledIcon(16, "#fff")} />}
      onClose={onClose}
    >
      <p style={{ margin: 0, fontSize: 15, lineHeight: 1.4, color: colors.body }}>{messages.pauseText}</p>
    </Popover>
  );
}
