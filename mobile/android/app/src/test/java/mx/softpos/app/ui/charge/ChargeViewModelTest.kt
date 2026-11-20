package mx.softpos.app.ui.charge

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.merchant.MerchantBalances
import mx.softpos.app.merchant.MerchantProfile
import mx.softpos.app.payments.CardPaymentIntent
import mx.softpos.app.payments.CardPaymentStatus
import mx.softpos.app.payments.FakeCardBackend
import mx.softpos.app.payments.MockTapToPayProvider
import mx.softpos.app.payments.TapToPayOutcome
import mx.softpos.app.payments.TapToPayProvider
import mx.softpos.app.payments.UnavailableTapToPayProvider
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class FakeMerchantApi(
    var profile: ApiResult<MerchantProfile> = ApiResult.Success(
        MerchantProfile("mer_1", "XAXX010101000", "Tacos Don Pepe", true, "646180157000000004"), 200
    ),
    var balances: ApiResult<MerchantBalances> = ApiResult.Success(MerchantBalances("mer_1", 150_000, 23_456), 200)
) : MerchantApi {
    var profileCalls = 0
    override fun getBalances(merchantId: String) = balances
    override fun getMerchant(merchantId: String): ApiResult<MerchantProfile> { profileCalls++; return profile }
}

@OptIn(ExperimentalCoroutinesApi::class)
class ChargeViewModelTest {
    private val dispatcher = StandardTestDispatcher()
    private val backend = FakeCardBackend()
    private val merchant = FakeMerchantApi()
    private var keys = 0

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    private fun mockTap(delayMs: Long = 10) = MockTapToPayProvider(backend, delayMs, dispatcher)

    private fun vm(
        tap: TapToPayProvider = mockTap(),
        merchantId: String? = "mer_1",
        poll: PollPolicy = PollPolicy(initialDelayMs = 100, maxDelayMs = 400, timeoutMs = 2_000)
    ) = ChargeViewModel(
        cardApi = backend,
        tapToPay = tap,
        merchantApi = merchant,
        merchantId = { merchantId },
        io = dispatcher,
        newIdempotencyKey = { "key_${++keys}" },
        newReference = { "0012345" },
        nowEpochSec = { 1_700_000_000L },
        poll = poll
    )

    private fun ChargeViewModel.type(vararg digits: Int) = digits.forEach { onDigit(it) }

    @Test
    fun keypadBuildsIntegerCents() {
        val vm = vm()
        vm.type(1, 2, 3, 4, 5)
        assertEquals(12_345L, vm.state.value.amountCents)
        vm.onBackspace()
        assertEquals(1_234L, vm.state.value.amountCents)
        vm.onClear()
        assertEquals(0L, vm.state.value.amountCents)
    }

    @Test
    fun cardChargeCapturedEndToEndThroughDevSimulator() = runTest(dispatcher) {
        val vm = vm()
        vm.type(2, 5, 0, 0, 0)
        vm.chargeWithCard()
        assertEquals(ChargeStep.ProcessingCard(CardStage.CREATING_INTENT), vm.state.value.step)
        advanceUntilIdle()

        val step = vm.state.value.step as ChargeStep.CardApproved
        assertEquals(25_000L, step.amountCents)
        assertEquals("cp_1", step.paymentId)
        assertEquals(listOf("tap:cp_1"), backend.simulated)
        assertEquals(CardPaymentStatus.CAPTURED, backend.payments.getValue("cp_1").status)
    }

    @Test
    fun declinedByBackendShowsDeclineCode() = runTest(dispatcher) {
        val vm = vm()
        vm.type(1, 5, 1) // $1.51 -> backend mock acquirer declines amounts ending in 51
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(ChargeStep.Entering, vm.state.value.step)
        assertEquals(ChargeError.DECLINED, vm.state.value.error)
        assertEquals("do_not_honor", vm.state.value.declineCode)
        assertEquals(151L, vm.state.value.amountCents)

        vm.chargeWithCard() // a new attempt after a decline uses a new key
        advanceUntilIdle()
        assertEquals(listOf("key_1", "key_2"), backend.intentKeys)
    }

    @Test
    fun networkErrorAnd502RetryWithTheSameKey() = runTest(dispatcher) {
        backend.intentErrors += ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")
        backend.intentErrors += ApiResult.Error(502, "ACQUIRER_UNAVAILABLE", "")
        val vm = vm()
        vm.type(5, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(ChargeError.NETWORK, vm.state.value.error)

        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(ChargeError.ACQUIRER_UNAVAILABLE, vm.state.value.error)

        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(listOf("key_1", "key_1", "key_1"), backend.intentKeys)
        assertTrue(vm.state.value.step is ChargeStep.CardApproved)
        assertEquals(1, backend.payments.size)

        vm.newCharge()
        vm.type(5, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals("key_2", backend.intentKeys.last())
    }

    @Test
    fun nonRetryableErrorsDropTheKey() = runTest(dispatcher) {
        backend.intentErrors += ApiResult.Error(503, "ACQUIRER_NOT_CONFIGURED", "")
        backend.intentErrors += ApiResult.Error(409, "IDEMPOTENCY_KEY_REUSED", "")
        backend.intentErrors += ApiResult.Error(403, "MERCHANT_NOT_APPROVED", "")
        val vm = vm()
        vm.type(1, 0, 0)
        vm.chargeWithCard(); advanceUntilIdle()
        assertEquals(ChargeError.CARD_NOT_CONFIGURED, vm.state.value.error)
        vm.chargeWithCard(); advanceUntilIdle()
        assertEquals(ChargeError.IDEMPOTENCY_CONFLICT, vm.state.value.error)
        vm.chargeWithCard(); advanceUntilIdle()
        assertEquals(ChargeError.NOT_APPROVED, vm.state.value.error)
        assertEquals(listOf("key_1", "key_2", "key_3"), backend.intentKeys)
    }

    @Test
    fun serverErrorsAndGatewayTimeoutsKeepTheKey() = runTest(dispatcher) {
        // 500 CARD_PAYMENT_STORE_FAILED can follow a stored intent; a proxy 504 has no
        // body at all. Either way the outcome is unknown: a new key could create a second intent.
        backend.intentErrors += ApiResult.Error(500, "CARD_PAYMENT_STORE_FAILED", "")
        backend.intentErrors += ApiResult.Error(504, "HTTP_504", "")
        backend.intentErrors += ApiResult.Error(502, "ACQUIRER_REJECTED", "")
        val vm = vm()
        vm.type(4, 0, 0)
        repeat(3) { vm.chargeWithCard(); advanceUntilIdle() }
        vm.chargeWithCard(); advanceUntilIdle()
        assertEquals(listOf("key_1", "key_1", "key_1", "key_2"), backend.intentKeys, "only the definitive rejection drops it")
        assertTrue(vm.state.value.step is ChargeStep.CardApproved)
    }

    @Test
    fun replayOfATappedIntentPollsInsteadOfRestartingTheSdk() = runTest(dispatcher) {
        // Backend replay of a non-CREATED intent: no client_secret, no new SDK session.
        backend.intentResponse = { it.copy(status = CardPaymentStatus.AUTHORIZED, clientSecret = "") }
        var collects = 0
        val tap = object : TapToPayProvider {
            override val isAvailable = true
            override suspend fun collect(intent: CardPaymentIntent): TapToPayOutcome { collects++; return TapToPayOutcome.Failed("x") }
        }
        val vm = vm(tap = tap)
        vm.type(1, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(0, collects)
        assertEquals(emptyList<String>(), backend.cancels, "an authorized payment is never canceled by the app")
        assertTrue(vm.state.value.step is ChargeStep.CardPending)
    }

    @Test
    fun pollingBacksOffAndTimesOutAsPending() = runTest(dispatcher) {
        // An SDK that "approves" but no webhook ever arrives: the status stays CREATED.
        val silentTap = object : TapToPayProvider {
            override val isAvailable = true
            override suspend fun collect(intent: CardPaymentIntent) = TapToPayOutcome.Approved
        }
        val vm = vm(tap = silentTap)
        vm.type(1, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertTrue(vm.state.value.step is ChargeStep.CardPending)
        // 100,150,225,337,400,400,400 = 2012 ms ≥ 2000 → 8 GETs, not one per tick.
        assertEquals(8, backend.getCalls)
        assertEquals(1_000L, PollPolicy(maxDelayMs = 1_000).next(800))
    }

    @Test
    fun cancelWhileWaitingForCardCancelsIntent() = runTest(dispatcher) {
        val vm = vm(tap = mockTap(delayMs = 10_000))
        vm.type(3, 0, 0)
        vm.chargeWithCard()
        runCurrent()
        assertEquals(ChargeStep.ProcessingCard(CardStage.WAITING_FOR_CARD), vm.state.value.step)
        vm.cancelCardCharge()
        advanceUntilIdle()
        assertEquals(ChargeStep.Entering, vm.state.value.step)
        assertEquals(ChargeError.CANCELLED, vm.state.value.error)
        assertEquals(listOf("cp_1"), backend.cancels)
        assertTrue("cancel:cp_1" in backend.simulated)
        assertFalse("tap:cp_1" in backend.simulated)
    }

    @Test
    fun simulatorUnavailableIsATapFailureAndCancelsIntent() = runTest(dispatcher) {
        backend.devMode = false
        val vm = vm()
        vm.type(1, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(ChargeError.TAP_FAILED, vm.state.value.error)
        assertEquals(listOf("cp_1"), backend.cancels)
    }

    @Test
    fun guardsBeforeCharging() = runTest(dispatcher) {
        val noAmount = vm()
        noAmount.chargeWithCard()
        assertEquals(ChargeError.NO_AMOUNT, noAmount.state.value.error)

        val noMerchant = vm(merchantId = null)
        noMerchant.type(1)
        noMerchant.chargeWithSpei()
        assertEquals(ChargeError.NO_MERCHANT, noMerchant.state.value.error)

        val noTap = vm(tap = UnavailableTapToPayProvider)
        assertEquals(false, noTap.state.value.tapAvailable)
        noTap.type(1)
        noTap.chargeWithCard()
        assertEquals(ChargeError.TAP_UNAVAILABLE, noTap.state.value.error)
    }

    @Test
    fun speiQrCarriesClabeAmountAndReference() = runTest(dispatcher) {
        val vm = vm()
        vm.type(9, 9, 0, 0)
        vm.chargeWithSpei()
        advanceUntilIdle()

        val step = vm.state.value.step as ChargeStep.SpeiQr
        assertEquals("646180157000000004", step.clabe)
        assertEquals("0012345", step.reference)
        val payload = Json.parseToJsonElement(step.qr.payloadJson).jsonObject
        assertEquals("646180157000000004", payload["clabe"]!!.jsonPrimitive.content)
        assertEquals(9_900L, payload["amount_cents"]!!.jsonPrimitive.long)
        assertEquals("SPEI", payload["type"]!!.jsonPrimitive.content)
        assertEquals(1_700_000_000L + ChargeViewModel.QR_TTL_SEC, step.qr.expiresAtEpochSec)

        vm.backToAmount()
        vm.chargeWithSpei()
        advanceUntilIdle()
        assertEquals(1, merchant.profileCalls, "merchant profile is cached")
    }

    @Test
    fun speiWithoutClabeIsRejected() = runTest(dispatcher) {
        merchant.profile = ApiResult.Success(MerchantProfile(id = "mer_1", name = "X"), 200)
        val vm = vm()
        vm.type(1)
        vm.chargeWithSpei()
        advanceUntilIdle()
        assertEquals(ChargeError.NO_CLABE, vm.state.value.error)
        assertEquals(ChargeStep.Entering, vm.state.value.step)
    }

    @Test
    fun keypadIsLockedWhileProcessing() = runTest(dispatcher) {
        val vm = vm()
        vm.type(1, 0, 0)
        vm.chargeWithCard()
        vm.onDigit(5)
        assertEquals(100L, vm.state.value.amountCents)
        advanceUntilIdle()
    }

    @Test
    fun mockProviderMirrorsBackendScenarios() = runTest(dispatcher) {
        val approved = (backend.createIntent("a", 100) as ApiResult.Success).data
        val declined = (backend.createIntent("b", 251) as ApiResult.Success).data
        assertEquals(TapToPayOutcome.Approved, mockTap(1).collect(approved))
        assertTrue(mockTap(1).collect(declined) is TapToPayOutcome.Declined)
        advanceTimeBy(1)
    }
}
