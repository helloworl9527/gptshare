-- Keep the displayed/current cycle duration aligned with the authoritative
-- redeemed_at/expires_at interval for cards created before extensions updated
-- duration_days.
UPDATE cards
SET duration_days = CAST(round(julianday(expires_at) - julianday(redeemed_at)) AS INTEGER),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE redeemed_at IS NOT NULL
  AND expires_at IS NOT NULL
  AND round(julianday(expires_at) - julianday(redeemed_at)) BETWEEN 1 AND 90
  AND duration_days != CAST(round(julianday(expires_at) - julianday(redeemed_at)) AS INTEGER);
