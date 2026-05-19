import EmptyState from "../_components/EmptyState";

export default function FailureGroupsPage() {
  return (
    <EmptyState
      icon="ti-alert-triangle"
      title="No failure groups yet"
      description="Mesedi clusters related agent failures into groups so the same problem doesn't page you a hundred times. Groups will appear here once your SDK starts sending data — eight failure classes are detected automatically."
    />
  );
}
