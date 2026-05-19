type EmptyStateProps = {
  icon: string;
  title: string;
  description: string;
};

export default function EmptyState({ icon, title, description }: EmptyStateProps) {
  return (
    <div className="p-8 max-w-5xl mx-auto">
      <div
        className="rounded-lg p-12 flex flex-col items-center text-center"
        style={{ background: "var(--surface)", border: "1px solid var(--border)" }}
      >
        <div
          className="w-12 h-12 rounded-lg flex items-center justify-center mb-4"
          style={{ background: "var(--surface-2)", color: "var(--accent)" }}
        >
          <i className={`ti ${icon}`} style={{ fontSize: "22px" }} aria-hidden="true" />
        </div>
        <h2 className="text-lg font-medium mb-2" style={{ color: "var(--text)" }}>
          {title}
        </h2>
        <p
          className="text-sm max-w-md"
          style={{ color: "var(--text-muted)", lineHeight: 1.6 }}
        >
          {description}
        </p>
      </div>
    </div>
  );
}
