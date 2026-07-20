-- onwardpg:assert booking_status_present
SELECT NOT EXISTS (
  SELECT 1
  FROM "app"."bookings"
  WHERE "status" IS NULL
);
