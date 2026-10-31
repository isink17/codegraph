package sample

open class Api {
    constructor()
    constructor(seed: Int)

    fun echo(value: String): String = value
    fun overloaded(value: String): String = value
    fun overloaded(value: Int): String = value.toString()
    @JvmName("renamed") fun named(value: String): String = value
    fun withDefault(value: String = "d"): String = value

    companion object {
        fun companionMethod(value: String): String = value
        @JvmStatic fun staticBridge(value: String): String = value
    }
}

internal class InternalApi { fun hiddenFromPublicContract(): String = "internal" }
private class PrivateApi { fun hiddenFromPublicContract(): String = "private" }

interface Getter<T> { fun get(): T }
class StringGetter : Getter<String> { override fun get(): String = "x" }

interface DefaultMethod {
    fun greeting(): String = "hello"
}
