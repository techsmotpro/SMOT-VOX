# Architecture Deep Dive

## Media Layer (Asterisk)

Handles SIP signaling, RTP streaming, codec negotiation (G.711, Opus).
Uses chan_websocket to stream raw PCM to Pipecat.

## Orchestration (Pipecat)

Manages real-time pipeline sequencing:
Transport → VAD → STT → LLM → Sentiment → TTS → Transport

## Post-Call Processing (n8n)

- Ingests transcript + tier + summary from Pipecat
- Writes to CRM via REST API
- Inserts record into PostgreSQL
- Triggers SMS alert on Tier 6 (Dangerous)
