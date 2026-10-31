package mx.softpos.app.ui

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class SoftPosComposeDescriptorTest {

    @Test
    fun `describe builds initial POS_TERMINAL render model with 4 bottom navigation tabs`() {
        val controller = SoftPosAppController()
        val model = SoftPosComposeDescriptor.describe(controller)

        assertEquals(AppScreen.POS_TERMINAL, model.activeScreen)
        assertEquals("Cobrar", model.titleEs)
        assertEquals("$0.00 MXN", model.formattedAmountMxn)
        assertFalse(model.isCheckoutReady)
        assertEquals(4, model.bottomNavTabs.size)
        assertTrue(model.bottomNavTabs.first { it.screen == AppScreen.POS_TERMINAL }.isSelected)
        assertFalse(model.bottomNavTabs.first { it.screen == AppScreen.TAX_VAULT }.isSelected)
    }

    @Test
    fun `describe updates formattedAmountMxn, isCheckoutReady, and active tab selection`() {
        val controller = SoftPosAppController()
        listOf(5, 0, 0, 0, 0).forEach { controller.posViewModel.onDigit(it) } // $500.00 MXN
        controller.navigateTo(AppScreen.TAX_VAULT)

        val model = SoftPosComposeDescriptor.describe(controller)
        assertEquals(AppScreen.TAX_VAULT, model.activeScreen)
        assertEquals("Bóveda SAT", model.titleEs)
        assertEquals("$500.00 MXN", model.formattedAmountMxn)
        assertTrue(model.isCheckoutReady)
        assertTrue(model.bottomNavTabs.first { it.screen == AppScreen.TAX_VAULT }.isSelected)
        assertFalse(model.bottomNavTabs.first { it.screen == AppScreen.POS_TERMINAL }.isSelected)
    }
}
