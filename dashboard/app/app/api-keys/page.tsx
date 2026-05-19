import EmptyState from "../_components/EmptyState";

export default function ApiKeysPage() {
  return (
    <EmptyState
      icon="ti-key"
      title="API keys"
      description="Generate and rotate API keys for your projects. Each key authenticates SDK calls against the Mesedi backend. Create separate keys per environment (dev, staging, prod) so you can revoke leaked credentials without breaking the rest."
    />
  );
}
