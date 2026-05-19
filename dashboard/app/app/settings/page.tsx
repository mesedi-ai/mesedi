import EmptyState from "../_components/EmptyState";

export default function SettingsPage() {
  return (
    <EmptyState
      icon="ti-settings"
      title="Settings"
      description="Project preferences, severity routing, retention windows, and team membership. Coming in a future slice — for now, configuration lives in environment variables on your backend."
    />
  );
}
