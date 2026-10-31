package mx.softpos.app.ui.tap

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.Random

class ScrambledKeypadModelTest {

    @Test
    fun `scrambled layout contains all digits 0 to 9 exactly once in a 12-key 3x4 grid`() {
        val model = ScrambledKeypadModel()
        val keys = model.keys

        // 3 columns x 4 rows = 12 keys
        assertEquals(12, keys.size)

        val digits = keys.filterIsInstance<KeypadKey.Digit>().map { it.value }
        assertEquals(10, digits.size)
        assertEquals((0..9).toSet(), digits.toSet())

        // Must include Backspace and Clear
        assertTrue(keys.any { it is KeypadKey.Backspace })
        assertTrue(keys.any { it is KeypadKey.Clear })
    }

    @Test
    fun `two consecutive instances generate different digit permutations`() {
        val model1 = ScrambledKeypadModel()
        val model2 = ScrambledKeypadModel()

        val digits1 = model1.keys.filterIsInstance<KeypadKey.Digit>().map { it.value }
        val digits2 = model2.keys.filterIsInstance<KeypadKey.Digit>().map { it.value }

        // With 10! = 3.6 million permutations, two random layouts should not match
        assertNotEquals(digits1, digits2)
    }

    @Test
    fun `scramble method produces a fresh permutation`() {
        val model = ScrambledKeypadModel()
        val originalDigits = model.keys.filterIsInstance<KeypadKey.Digit>().map { it.value }

        val scrambled = model.scramble()
        val newDigits = scrambled.keys.filterIsInstance<KeypadKey.Digit>().map { it.value }

        assertEquals(12, scrambled.keys.size)
        assertEquals((0..9).toSet(), newDigits.toSet())
        assertNotEquals(originalDigits, newDigits)
    }

    @Test
    fun `deterministic random generator produces repeatable order for unit testing`() {
        val seed = 42L
        val model1 = ScrambledKeypadModel(Random(seed))
        val model2 = ScrambledKeypadModel(Random(seed))

        assertEquals(model1.keys, model2.keys)
    }
}
