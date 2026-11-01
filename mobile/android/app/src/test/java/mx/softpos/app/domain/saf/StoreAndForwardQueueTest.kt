package mx.softpos.app.domain.saf

import kotlinx.coroutines.runBlocking
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class StoreAndForwardQueueTest {

    @Test
    fun `enqueueTransaction accepts amounts up to 400 MXN and rejects higher amounts`() {
        val queue = StoreAndForwardQueue()

        // 400.00 MXN = 40,000 cents -> Allowed
        val tx1 = queue.enqueue(
            id = "TX-OFFLINE-01",
            merchantId = "MERCHANT_01",
            amountCents = 40_000L,
            payload = "RAW_EMV_TLV_PAYLOAD_01"
        )
        assertEquals(1, queue.size())
        assertEquals("TX-OFFLINE-01", tx1.id)

        // 400.01 MXN = 40,001 cents -> Rejected
        assertThrows(AmountExceedsOfflineLimitException::class.java) {
            queue.enqueue(
                id = "TX-OFFLINE-02",
                merchantId = "MERCHANT_01",
                amountCents = 40_001L,
                payload = "RAW_EMV_TLV_PAYLOAD_02"
            )
        }
        assertEquals(1, queue.size())
    }

    @Test
    fun `encrypted payload at rest differs from plaintext and is reversible`() {
        val queue = StoreAndForwardQueue()
        val plainPayload = "SENSITIVE_CARD_TRACK2_AND_EMV_DATA_4000123456789010"

        val stored = queue.enqueue(
            id = "TX-SEC-01",
            merchantId = "MERCHANT_01",
            amountCents = 15_000L,
            payload = plainPayload
        )

        // Verifies encrypted at rest (not plain)
        assertNotEquals(plainPayload, stored.encryptedPayload)
        assertFalse(stored.encryptedPayload.contains("4000123456789010"))

        val decrypted = queue.decryptPayload(stored.encryptedPayload)
        assertEquals(plainPayload, decrypted)
    }

    @Test
    fun `dispatchAll flushes queue in FIFO order on network reconnection`() = runBlocking {
        val queue = StoreAndForwardQueue()

        queue.enqueue("TX-1", "M1", 10_000L, "PAYLOAD_1")
        queue.enqueue("TX-2", "M1", 20_000L, "PAYLOAD_2")
        queue.enqueue("TX-3", "M1", 30_000L, "PAYLOAD_3")
        assertEquals(3, queue.size())

        val dispatchedList = mutableListOf<String>()
        val result = queue.dispatchAll { id, _, _, payload ->
            dispatchedList.add("$id:$payload")
            true
        }

        assertTrue(result.isFullyFlushed)
        assertEquals(3, result.dispatchedCount)
        assertEquals(0, result.remainingCount)
        assertEquals(0, queue.size())

        // FIFO verification
        assertEquals("TX-1:PAYLOAD_1", dispatchedList[0])
        assertEquals("TX-2:PAYLOAD_2", dispatchedList[1])
        assertEquals("TX-3:PAYLOAD_3", dispatchedList[2])
    }

    @Test
    fun `dispatchAll halts on failure and preserves remaining transactions in FIFO order`() = runBlocking {
        val queue = StoreAndForwardQueue()

        queue.enqueue("TX-A", "M1", 10_000L, "PAYLOAD_A")
        queue.enqueue("TX-B", "M1", 20_000L, "PAYLOAD_B")
        queue.enqueue("TX-C", "M1", 30_000L, "PAYLOAD_C")

        val dispatchedList = mutableListOf<String>()
        val result = queue.dispatchAll { id, _, _, payload ->
            if (id == "TX-B") {
                // Simulate backend transient failure or network drop
                false
            } else {
                dispatchedList.add("$id:$payload")
                true
            }
        }

        assertFalse(result.isFullyFlushed)
        assertEquals(1, result.dispatchedCount) // Only TX-A succeeded
        assertEquals(2, result.remainingCount)  // TX-B and TX-C remain
        assertEquals(2, queue.size())

        val remaining = queue.peekAll()
        assertEquals("TX-B", remaining[0].id)
        assertEquals("TX-C", remaining[1].id)
    }
}
