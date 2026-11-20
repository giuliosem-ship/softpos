package mx.softpos.app.ui.charge

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import mx.softpos.app.core.network.ApiResult
import mx.softpos.app.core.network.NETWORK_ERROR_CODE
import mx.softpos.app.core.network.NETWORK_ERROR_STATUS
import mx.softpos.app.merchant.MerchantBalances
import mx.softpos.app.merchant.MerchantApi
import mx.softpos.app.merchant.MerchantProfile
import mx.softpos.app.payments.CardPaymentIntent
import mx.softpos.app.payments.CardPaymentsApi
import mx.softpos.app.payments.InMemoryPaymentsBackend
import mx.softpos.app.payments.MockTapToPayProvider
import mx.softpos.app.payments.TapToPayOutcome
import mx.softpos.app.payments.TapToPayProvider
import mx.softpos.app.payments.UnavailableTapToPayProvider
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
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
    private val backend = InMemoryPaymentsBackend()
    private val merchant = FakeMerchantApi()
    private var keys = 0

    @BeforeEach fun setUp() = Dispatchers.setMain(dispatcher)
    @AfterEach fun tearDown() = Dispatchers.resetMain()

    private fun vm(
        cardApi: CardPaymentsApi = backend,
        tap: TapToPayProvider = MockTapToPayProvider(delayMs = 10, onOutcome = backend::onTapOutcome),
        merchantId: String? = "mer_1"
    ) = ChargeViewModel(
        cardApi = cardApi,
        tapToPay = tap,
        merchantApi = merchant,
        merchantId = { merchantId },
        io = dispatcher,
        newIdempotencyKey = { "key_${++keys}" },
        newReference = { "0012345" },
        nowEpochSec = { 1_700_000_000L }
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
    fun cardChargeApprovedEndToEnd() = runTest(dispatcher) {
        val vm = vm()
        vm.type(2, 5, 0, 0, 0)
        vm.chargeWithCard()
        assertEquals(ChargeStep.ProcessingCard(CardStage.CREATING_INTENT), vm.state.value.step)
        advanceUntilIdle()

        val step = vm.state.value.step as ChargeStep.CardApproved
        assertEquals(25_000L, step.amountCents)
        val sales = (backend.listSales("mer_1") as ApiResult.Success).data.sales
        assertEquals(1, sales.size)
        assertEquals("succeeded", sales.single().status)
    }

    @Test
    fun declinedCardReturnsToKeypadWithError() = runTest(dispatcher) {
        val vm = vm()
        vm.type(1, 1, 3) // $1.13 -> mock declines amounts ending in 13 centavos
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(ChargeStep.Entering, vm.state.value.step)
        assertEquals(ChargeError.DECLINED, vm.state.value.error)
        assertEquals(113L, vm.state.value.amountCents)
    }

    @Test
    fun retryAfterNetworkErrorReusesIdempotencyKey() = runTest(dispatcher) {
        val seenKeys = mutableListOf<String>()
        var fail = true
        val api = object : CardPaymentsApi {
            override fun createIntent(idempotencyKey: String, amountCents: Long): ApiResult<CardPaymentIntent> {
                seenKeys += idempotencyKey
                if (fail) return ApiResult.Error(NETWORK_ERROR_STATUS, NETWORK_ERROR_CODE, "")
                return backend.createIntent(idempotencyKey, amountCents)
            }
            override fun getPayment(id: String) = backend.getPayment(id)
        }
        val vm = vm(cardApi = api)
        vm.type(5, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(ChargeError.NETWORK, vm.state.value.error)

        fail = false
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals(listOf("key_1", "key_1"), seenKeys)
        assertTrue(vm.state.value.step is ChargeStep.CardApproved)

        vm.newCharge()
        vm.type(5, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertEquals("key_2", seenKeys.last())
    }

    @Test
    fun tapWithoutBackendConfirmationIsPending() = runTest(dispatcher) {
        // Tap approved but no webhook yet: status stays requires_tap.
        val vm = vm(tap = MockTapToPayProvider(delayMs = 1))
        vm.type(1, 0, 0)
        vm.chargeWithCard()
        advanceUntilIdle()
        assertTrue(vm.state.value.step is ChargeStep.CardPending)
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
    fun mockProviderOutcomes() = runTest(dispatcher) {
        val intent = CardPaymentIntent("cpi_1", "s", "requires_tap", 100)
        assertTrue(MockTapToPayProvider(1).collect(intent, 100) is TapToPayOutcome.Approved)
        assertTrue(MockTapToPayProvider(1).collect(intent, 213) is TapToPayOutcome.Declined)
    }
}
