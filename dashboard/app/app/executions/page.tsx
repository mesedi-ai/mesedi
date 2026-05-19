import EmptyState from "../_components/EmptyState";

export default function ExecutionsPage() {
  return (
    <EmptyState
      icon="ti-list-tree"
      title="No executions yet"
      description="Every agent run wrapped with the Mesedi SDK appears here — full trace of steps, tool calls, model outputs, and runtime cost. Drill into any execution to see detected failures and the playbook fix."
    />
  );
}
