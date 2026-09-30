/*
 * Local approval card. Beautiful UI "Approval Card" was not integrated: its
 * source imports @/components/atoms/Button and @/components/primitives/GlideMenu,
 * which are not published on https://www.beautifului.dev/ (404) and could not
 * be obtained. This plain component uses only trusted page controls; the
 * decision set is fixed by the server contract, not by model output.
 */
const DECISIONS: { value: string; label: string; tone: string }[] = [
  { value: "allowed-once", label: "批准一次", tone: "bg-ink text-surface" },
  { value: "rejected", label: "拒绝", tone: "bg-surface text-ink shadow-btn" },
];

export default function ApprovalCard({
  question,
  options,
  busy,
  error,
  onDecide,
}: {
  question: string;
  options: string[];
  busy: boolean;
  error: string;
  onDecide: (decision: string) => void;
}) {
  return (
    <section aria-label="待审批操作" className="w-full rounded-card bg-surface p-3 shadow-card">
      <p className="text-[12px] font-medium text-orange">需要审批</p>
      <p className="mt-1 text-[13px] leading-normal break-words text-ink">{question}</p>
      <div className="mt-2.5 flex gap-2">
        {DECISIONS.filter((d) => options.includes(d.value)).map((d) => (
          <button
            key={d.value}
            type="button"
            disabled={busy}
            onClick={() => onDecide(d.value)}
            className={`rounded-control px-3 py-1.5 text-[12.5px] font-medium disabled:opacity-50 ${d.tone}`}
          >
            {d.label}
          </button>
        ))}
      </div>
      {error && (
        <p role="alert" className="mt-2 text-[12px] text-red">
          {error}
        </p>
      )}
    </section>
  );
}
