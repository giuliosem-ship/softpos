package mx.softpos.app.nfc

import kotlinx.coroutines.test.runTest
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class TapPaymentOrchestratorTest {

    private class FakeTapPaymentProvider(
        private val result: TapPaymentResult
    ) : TapPaymentProvider {
        override suspend fun collectContactlessPayment(amountCents: Long, merchantId: String): TapPaymentResult {
            return result
        }
    }

    private class FakeEmvBackendGateway(
        private val authorizeResult: Boolean = true,
        private val settleResult: Boolean = true,
        private val reverseResult: Boolean = true
    ) : EmvBackendGateway {
        var authorizeCalls = 0
        var settleCalls = 0
        var reverseCalls = 0

        override suspend fun authorize(paymentId: String, amountCents: Long, tlvHex: String): Boolean {
            authorizeCalls++
            return authorizeResult
        }

        override suspend fun settle(paymentId: String): Boolean {
            settleCalls++
            return settleResult
        }

        override suspend fun reverse(paymentId: String): Boolean {
            reverseCalls++
            return reverseResult
        }
    }

    @Test
    fun `processTap rejects non-positive amountCents`() = runTest {
        val provider = FakeTapPaymentProvider(TapPaymentResult.Declined("ERR", "Declined"))
        val gateway = FakeEmvBackendGateway()
        val orchestrator = TapPaymentOrchestrator(provider, gateway)

        assertThrows(IllegalArgumentException::class.java) {
            kotlinx.coroutines.runBlocking {
                orchestrator.processTap(0L, "merch_1")
            }
        }
    }

    @Test
    fun `processTap completes full lifecycle when card reading, authorization, and settlement succeed`() = runTest {
        val approved = TapPaymentResult.Approved(
            paymentId = "pay_1001",
            maskedPan = "415231******9012",
            cryptogramHex = "AABBCCDD11223344",
            tlvPayloadHex = "9F0206000000010000"
        )
        val gateway = FakeEmvBackendGateway(authorizeResult = true, settleResult = true)
        val orchestrator = TapPaymentOrchestrator(FakeTapPaymentProvider(approved), gateway)

        assertEquals(TapStage.IDLE, orchestrator.state.value.stage)

        val finalState = orchestrator.processTap(15_000L, "merch_mx_1")

        assertEquals(TapStage.COMPLETED, finalState.stage)
        assertEquals(15_000L, finalState.amountCents)
        assertEquals("pay_1001", finalState.paymentId)
        assertEquals("415231******9012", finalState.maskedPan)
        assertNull(finalState.errorMessage)
        assertEquals(1, gateway.authorizeCalls)
        assertEquals(1, gateway.settleCalls)
        assertEquals(0, gateway.reverseCalls)

        orchestrator.reset()
        assertEquals(TapStage.IDLE, orchestrator.state.value.stage)
    }

    @Test
    fun `processTap transitions to REVERSED and triggers gateway reverse when settlement fails`() = runTest {
        val approved = TapPaymentResult.Approved(
            paymentId = "pay_1002",
            maskedPan = "541275******1234",
            cryptogramHex = "1122334455667788",
            tlvPayloadHex = "9F0206000000025000"
        )
        val gateway = FakeEmvBackendGateway(authorizeResult = true, settleResult = false, reverseResult = true)
        val orchestrator = TapPaymentOrchestrator(FakeTapPaymentProvider(approved), gateway)

        val finalState = orchestrator.processTap(25_000L, "merch_mx_1")

        assertEquals(TapStage.REVERSED, finalState.stage)
        assertEquals("pay_1002", finalState.paymentId)
        assertEquals(1, gateway.authorizeCalls)
        assertEquals(1, gateway.settleCalls)
        assertEquals(1, gateway.reverseCalls)
    }

    @Test
    fun `processTap transitions to FAILED when card is declined or authorization fails`() = runTest {
        val declinedProvider = FakeTapPaymentProvider(TapPaymentResult.Declined("51", "Insufficient funds"))
        val gateway1 = FakeEmvBackendGateway()
        val orchestrator1 = TapPaymentOrchestrator(declinedProvider, gateway1)

        val state1 = orchestrator1.processTap(10_000L, "merch_mx_1")
        assertEquals(TapStage.FAILED, state1.stage)
        assertEquals("Insufficient funds", state1.errorMessage)
        assertEquals(0, gateway1.authorizeCalls)

        val approved = TapPaymentResult.Approved(
            paymentId = "pay_1003",
            maskedPan = "415231******9012",
            cryptogramHex = "AABBCCDD",
            tlvPayloadHex = "9F0206000000010000"
        )
        val gateway2 = FakeEmvBackendGateway(authorizeResult = false)
        val orchestrator2 = TapPaymentOrchestrator(FakeTapPaymentProvider(approved), gateway2)

        val state2 = orchestrator2.processTap(10_000L, "merch_mx_1")
        assertEquals(TapStage.FAILED, state2.stage)
        assertEquals("Authorization declined", state2.errorMessage)
        assertEquals(1, gateway2.authorizeCalls)
        assertEquals(0, gateway2.settleCalls)
    }
}
