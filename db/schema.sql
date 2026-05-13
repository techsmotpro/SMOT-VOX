CREATE TABLE call_logs (
    id SERIAL PRIMARY KEY,
    call_sid VARCHAR(64) UNIQUE NOT NULL,
    customer_phone VARCHAR(20),
    direction VARCHAR(10) CHECK (direction IN ('inbound', 'outbound')),
    started_at TIMESTAMP DEFAULT NOW(),
    ended_at TIMESTAMP,
    duration_sec INTEGER,
    transcript TEXT,
    transcript_json JSONB,
    sentiment_tier INTEGER CHECK (sentiment_tier BETWEEN 1 AND 6),
    sentiment_label VARCHAR(20),
    summary TEXT,
    audio_path VARCHAR(255),
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE alerts (
    id SERIAL PRIMARY KEY,
    call_log_id INTEGER REFERENCES call_logs(id),
    alert_type VARCHAR(20) DEFAULT 'tier6_dangerous',
    sent_at TIMESTAMP DEFAULT NOW(),
    recipient_phone VARCHAR(20)
);
