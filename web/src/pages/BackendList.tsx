import { type Endpoint } from "../api";

export default function BackendList({ endpoint }: { endpoint: Endpoint }) {
  const backends = endpoint.backends ?? [];
  if (backends.length === 0 && endpoint.entry_backend) {
    return (
      <span className="detail mono" title="The quota is counted on the model's entry route. The cluster's own routes have no backend for it.">
        {endpoint.entry_backend}
      </span>
    );
  }
  if (backends.length === 0) {
    return (
      <span className="tag warn" title="This cluster does not serve the model from an AIServiceBackend, so a quota cannot be attached here.">
        no quota here
      </span>
    );
  }
  return (
    <>
      <span className="detail mono">
        {backends.map((b) => (b.namespace ? `${b.namespace}/${b.name}` : b.name)).join(", ")}
      </span>
      {backends.some((b) => !b.override) && (
        <span
          className="tag warn"
          title="The route sets no modelNameOverride for this backend. The gateway documents quota matching only against modelNameOverride, so check that the quota takes effect."
        >
          quota unverified
        </span>
      )}
    </>
  );
}
