-- Index the alerts still firing, in the listing's order.
--
-- "Alerts, status firing" counted its total by reading the table front to back
-- until it had 10 001 matches (store.ListTotalCap). Firing alerts are the
-- newest few percent, at the far end of that read: on a 1.4-million-row table
-- the count took 0.7 s alone and the page up to 1.8 s under load. A partial
-- index on the firing rows answers both the count and the page from a few
-- thousand index entries, and costs ingest only for the alerts it covers.

CREATE INDEX IF NOT EXISTS nxs_anomaly_alerts_firing_list_order_idx
    ON nxs_anomaly_alerts (received_at DESC NULLS LAST, id DESC)
    WHERE status = 'firing';
