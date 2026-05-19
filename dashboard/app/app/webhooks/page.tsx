import EmptyState from "../_components/EmptyState";

export default function WebhooksPage() {
  return (
    <EmptyState
      icon="ti-webhook"
      title="No webhooks configured"
      description="Mesedi fires a webhook the first time a new failure class appears so you don't drown in repeat alerts. Add a webhook URL here and route by severity — escalation_critical, escalation_warning, or info."
    />
  );
}
