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
CREATE OR REPLACE VIEW tool_events AS
SELECT execution_id, event_type, sequence, timestamp, payload
  FROM events
 WHERE event_type = 'tool_call'
UNION ALL
SELECT execution_id, event_type, sequence, timestamp,
       jsonb_build_object(
         'tool_name', COALESCE(payload::jsonb->>'server_name', '') || '/' ||
                      COALESCE(payload::jsonb->>'method', ''),
         'status', CASE
                     WHEN COALESCE(payload::jsonb->>'error', '') <> ''
                       OR COALESCE(payload::jsonb->>'error_class', '') <> ''
                     THEN 'failed' ELSE 'ok' END,
         'exception_type', payload::jsonb->>'error_class',
         'exception_message', payload::jsonb->>'error',
         'arguments', payload::jsonb->'arguments',
         'return_value', payload::jsonb->'return_value',
         'input_schema_hash', payload::jsonb->>'input_schema_hash',
         'tool_description', payload::jsonb->>'tool_description',
         'server_name', payload::jsonb->>'server_name',
         'method', payload::jsonb->>'method',
         'server_url', payload::jsonb->>'server_url',
         'latency_ms', payload::jsonb->'latency_ms',
         'effects', payload::jsonb->'effects'
       )::text AS payload
  FROM events
 WHERE event_type = 'mcp_call';
