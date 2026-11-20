-- 010_card_payments_acquirer.down.sql
DROP TABLE IF EXISTS acquirer_reconciliation_items;
DROP TABLE IF EXISTS acquirer_payouts;
DROP TABLE IF EXISTS card_payment_refunds;
-- Settlement rows of card payments must be removed before the FK can return.
DELETE FROM payment_settlements WHERE payment_id NOT IN (SELECT id FROM emv_payments);
DROP TABLE IF EXISTS card_payments;
ALTER TABLE payment_settlements ADD CONSTRAINT payment_settlements_payment_id_fkey
    FOREIGN KEY (payment_id) REFERENCES emv_payments(id) ON DELETE RESTRICT;
