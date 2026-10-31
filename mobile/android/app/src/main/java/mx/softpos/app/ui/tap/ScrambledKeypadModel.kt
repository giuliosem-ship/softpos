package mx.softpos.app.ui.tap

import java.security.SecureRandom
import java.util.Random

sealed interface KeypadKey {
    data class Digit(val value: Int) : KeypadKey
    data object Backspace : KeypadKey
    data object Clear : KeypadKey
}

/**
 * ScrambledKeypadModel implements PCI MPoC SCRP (Software Component for PIN Verification)
 * randomized keypad layouts to mitigate screen-recording, observation, and grease-mark attacks.
 */
class ScrambledKeypadModel(
    random: Random = SecureRandom()
) {
    val keys: List<KeypadKey>

    init {
        // Generate a random permutation of digits 0..9
        val shuffledDigits = (0..9).toMutableList()
        // Fisher-Yates shuffle with provided secure random
        for (i in shuffledDigits.size - 1 downTo 1) {
            val j = random.nextInt(i + 1)
            val temp = shuffledDigits[i]
            shuffledDigits[i] = shuffledDigits[j]
            shuffledDigits[j] = temp
        }

        // Layout: 3 columns x 4 rows (12 keys total)
        // Row 1: slots 0, 1, 2 (digits)
        // Row 2: slots 3, 4, 5 (digits)
        // Row 3: slots 6, 7, 8 (digits)
        // Row 4: Clear, slot 9 (10th digit), Backspace
        val grid = ArrayList<KeypadKey>(12)
        for (i in 0 until 9) {
            grid.add(KeypadKey.Digit(shuffledDigits[i]))
        }
        grid.add(KeypadKey.Clear)
        grid.add(KeypadKey.Digit(shuffledDigits[9]))
        grid.add(KeypadKey.Backspace)

        keys = grid.toList()
    }

    fun scramble(random: Random = SecureRandom()): ScrambledKeypadModel {
        return ScrambledKeypadModel(random)
    }
}
