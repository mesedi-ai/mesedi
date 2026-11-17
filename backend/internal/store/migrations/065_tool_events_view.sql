-- 065: one view of every tool call, ordinary or MCP.
--
-- Until 2026-11-17 every tool check (failures, return-shape drift,
-- description and definition drift, pins, sandbox patterns, return
-- statistics) read only event_type 'tool_call'. MCP calls, reported as
-- 'mcp_call' with different field names, were read by no check at all
-- (the outside review's round 3 period; radar gap G29). This view
-- presents each MCP call under the field names the checks already read,
-- so the checks change only their FROM clause. Stored payloads are never
-- rewritten: they are digested into the evidence chain as sent.
--
-- An MCP tool is named server/method, for example github/read_file. A
-- slash, not a colon, because failure-group signatures already use
-- colons as separators. An MCP call failed when it reports an error or
-- an error class.
CREATE VIEW IF NOT EXISTS tool_events AS
SELECT execution_id, event_type, sequence, timestamp, payload
  FROM events
 WHERE event_type = 'tool_call'
UNION ALL
SELECT execution_id, event_type, sequence, timestamp,
       json_object(
         'tool_name', COALESCE(json_extract(payload, '$.server_name'), '') || '/' ||
                      COALESCE(json_extract(payload, '$.method'), ''),
         'status', CASE
                     WHEN COALESCE(json_extract(payload, '$.error'), '') <> ''
                       OR COALESCE(json_extract(payload, '$.error_class'), '') <> ''
                     THEN 'failed' ELSE 'ok' END,
         'exception_type', json_extract(payload, '$.error_class'),
         'exception_message', json_extract(payload, '$.error'),
         'arguments', payload -> '$.arguments',
         'return_value', payload -> '$.return_value',
         'input_schema_hash', json_extract(payload, '$.input_schema_hash'),
         'tool_description', json_extract(payload, '$.tool_description'),
         'server_name', json_extract(payload, '$.server_name'),
         'method', json_extract(payload, '$.method'),
         'server_url', json_extract(payload, '$.server_url'),
         'latency_ms', json_extract(payload, '$.latency_ms'),
         'effects', payload -> '$.effects'
       ) AS payload
  FROM events
 WHERE event_type = 'mcp_call';
