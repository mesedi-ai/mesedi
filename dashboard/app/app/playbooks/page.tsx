import EmptyState from "../_components/EmptyState";

export default function PlaybooksPage() {
  return (
    <EmptyState
      icon="ti-book"
      title="Tier 1 Playbooks"
      description="Per-failure-class canonical fix descriptions, shipped with Mesedi v1. When a failure group fires, the matching playbook surfaces here with the recommended fix. Tier 3 (auto-fix) is on the v2 roadmap."
    />
  );
}
