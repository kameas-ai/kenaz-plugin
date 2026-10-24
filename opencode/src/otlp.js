// Encodes adapter records as an OTLP/HTTP JSON logs export
// (ExportLogsServiceRequest), the form the relay's /v1/logs accepts.

export const SERVICE_NAME = "opencode";
export const SCOPE_NAME = "kenaz-opencode";

function value(v) {
  switch (typeof v) {
    case "boolean":
      return { boolValue: v };
    case "number":
      return Number.isInteger(v) ? { intValue: String(v) } : { doubleValue: v };
    default:
      return { stringValue: String(v) };
  }
}

function attributes(obj) {
  return Object.keys(obj)
    .sort()
    .map((key) => ({ key, value: value(obj[key]) }));
}

/** toLogsData(records, {serviceVersion}) returns the export request body. */
export function toLogsData(records, { serviceVersion = "" } = {}) {
  const resource = { "service.name": SERVICE_NAME };
  if (serviceVersion) resource["service.version"] = serviceVersion;
  return {
    resourceLogs: [
      {
        resource: { attributes: attributes(resource) },
        scopeLogs: [
          {
            scope: { name: SCOPE_NAME },
            logRecords: records.map((r) => {
              const nanos = String(BigInt(Math.floor(r.time)) * 1000000n);
              return {
                timeUnixNano: nanos,
                observedTimeUnixNano: nanos,
                eventName: r.name,
                attributes: attributes(r.attrs),
              };
            }),
          },
        ],
      },
    ],
  };
}
