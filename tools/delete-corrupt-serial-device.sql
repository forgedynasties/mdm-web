-- Run on live 24 Sep 2026 after the corrupt-serial refusal deployed (76eec6d).
-- Delete the corrupted-serial device record "androidboot.baseband=msm" and all its data.
-- Several T7s whose serial read corrupt were merged into this one record (19 boots, 29 IP
-- addresses). Run only after the server refuses corrupt serials, or it comes straight back.
\timing on
\set ON_ERROR_STOP on
SELECT id, serial_number, last_seen_at FROM devices WHERE serial_number = 'androidboot.baseband=msm';
\echo samples, in batches of 50000 (each its own transaction)
DO $$
DECLARE dev uuid; n bigint; total bigint := 0;
BEGIN
  SELECT id INTO dev FROM devices WHERE serial_number = 'androidboot.baseband=msm';
  IF dev IS NULL THEN RAISE NOTICE 'no such device'; RETURN; END IF;
  LOOP
    DELETE FROM device_samples WHERE (device_id, at) IN
      (SELECT device_id, at FROM device_samples WHERE device_id = dev LIMIT 50000);
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
    COMMIT;
    EXIT WHEN n = 0;
  END LOOP;
  RAISE NOTICE 'deleted % sample(s)', total;
END $$;
\echo the device itself (state events, build history, alerts and the rest go with it)
DELETE FROM devices WHERE serial_number = 'androidboot.baseband=msm';
SELECT count(*) AS left FROM devices WHERE serial_number = 'androidboot.baseband=msm';
\echo Done.
