package org.codegraph.jvm.binary

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertNotNull
import kotlin.test.assertTrue
import sample.Api
import sample.Caller
import org.objectweb.asm.ClassReader
import org.objectweb.asm.ClassWriter
import org.objectweb.asm.Opcodes
import org.objectweb.asm.tree.ClassNode

class InspectorTest {
    private fun classBytes(name: String): ByteArray =
        requireNotNull(javaClass.getResourceAsStream("/$name.class")).use { it.readBytes() }

    @Test
    fun verifiesOrdinaryMemberAndExactOverloads() {
        val result = inspect(classBytes("sample/Api"))
        assertEquals("sample/Api", result.classfile["owner"])
        assertEquals("supported", result.kotlin_metadata.state)
        assertEquals(listOf(2, 3, 0), result.kotlin_metadata.version)
        assertEquals(
            setOf(
                "echo(Ljava/lang/String;)Ljava/lang/String;",
                "overloaded(Ljava/lang/String;)Ljava/lang/String;",
                "overloaded(I)Ljava/lang/String;",
                "renamed(Ljava/lang/String;)Ljava/lang/String;",
                "withDefault(Ljava/lang/String;)Ljava/lang/String;",
            ),
            result.methods.map { it.jvm_name + it.descriptor }.toSet(),
        )
        val echo = result.methods.single { it.kotlin_name == "echo" }
        assertEquals("echo", echo.jvm_name)
        assertEquals("unique", echo.metadata_association)
        assertEquals("verified", echo.binary_signature_state)
        assertEquals("exact", echo.confidence)
        assertEquals(5, result.unassociated_method_count)
        assertEquals("unknown", result.trust.current_build_freshness)
        assertEquals("unknown", result.trust.compiler_input_completeness)
    }

    @Test
    fun doesNotAssociateConstructorsDefaultHelpersOrStaticBridge() {
        val result = inspect(classBytes("sample/Api"))
        assertFalse(result.methods.any { it.jvm_name == "<init>" || it.jvm_name.endsWith("$" + "default") })
        assertFalse(result.methods.any { it.jvm_name == "staticBridge" })
        assertFalse(result.methods.any { it.jvm_name == "companionMethod" })
    }

    @Test
    fun excludesCompanionsInterfacesAndFileFacades() {
        val companion = inspect(classBytes("sample/Api$" + "Companion"))
        assertEquals("unsupported_class_shape", companion.kotlin_metadata.state)
        assertTrue(companion.methods.isEmpty())

        val iface = inspect(classBytes("sample/DefaultMethod"))
        assertEquals("unsupported_class_shape", iface.kotlin_metadata.state)
        assertTrue(iface.methods.isEmpty())

        val facade = inspect(classBytes("sample/Functions"))
        assertEquals("unsupported_kind", facade.kotlin_metadata.state)
        assertTrue(facade.methods.isEmpty())
    }

    @Test
    fun leavesBridgeWithoutMetadataAssociation() {
        val result = inspect(classBytes("sample/StringGetter"))
        assertEquals(listOf("get()Ljava/lang/String;"), result.methods.map { it.jvm_name + it.descriptor })
        assertEquals(2, result.unassociated_method_count)
    }

    @Test
    fun compiledJavaCallerUsesTheVerifiedMethod() {
        assertEquals("ok", Caller().call(Api()))
    }

    @Test
    fun rejectsMetadataMethodMissingFromClassfile() {
        val node = readNode(classBytes("sample/Api"))
        val echo = node.methods.single { it.name == "echo" }
        echo.name = "echo_bytecode_mismatch"
        assertFailsWith<Exception> { inspect(writeNode(node)) }
    }

    @Test
    fun rejectsUnsupportedMetadataVersionBeforeParsing() {
        val node = readNode(classBytes("sample/Api"))
        val metadata = metadataAnnotation(node)
        val index = metadata.values!!.indexOf("mv")
        metadata.values!![index + 1] = listOf(999, 0, 0)
        assertFailsWith<Exception> { inspect(writeNode(node)) }
    }

    @Test
    fun rejectsCorruptMetadata() {
        val node = readNode(classBytes("sample/Api"))
        val metadata = metadataAnnotation(node)
        val index = metadata.values!!.indexOf("d1")
        metadata.values!![index + 1] = listOf("not-kotlin-metadata")
        assertFailsWith<Exception> { inspect(writeNode(node)) }
    }

    @Test
    fun rejectsAmbiguousDuplicateClassfileSignature() {
        val node = readNode(classBytes("sample/Api"))
        val original = node.methods.single { it.name == "echo" }
        val duplicate = org.objectweb.asm.tree.MethodNode(
            Opcodes.ASM9,
            original.access,
            original.name,
            original.desc,
            original.signature,
            original.exceptions?.toTypedArray(),
        )
        node.methods.add(duplicate)
        assertFailsWith<Exception> { inspect(writeNode(node)) }
    }

    @Test
    fun acceptsClassfileMethodsWithoutKotlinMetadataOnlyAsUnassociated() {
        val writer = ClassWriter(0)
        writer.visit(Opcodes.V17, Opcodes.ACC_PUBLIC, "sample/Plain", null, "java/lang/Object", null)
        writer.visitMethod(Opcodes.ACC_PUBLIC, "run", "()V", null, null).visitEnd()
        writer.visitEnd()
        val result = inspect(writer.toByteArray())
        assertEquals("absent", result.kotlin_metadata.state)
        assertTrue(result.methods.isEmpty())
        assertEquals(1, result.unassociated_method_count)
    }

    @Test
    fun rejectsMalformedClassfile() {
        assertFailsWith<Exception> { inspect(byteArrayOf(0, 1, 2, 3)) }
    }

    private fun readNode(bytes: ByteArray): ClassNode =
        ClassNode(Opcodes.ASM9).also { ClassReader(bytes).accept(it, 0) }

    private fun writeNode(node: ClassNode): ByteArray =
        ClassWriter(0).also(node::accept).toByteArray()

    private fun metadataAnnotation(node: ClassNode) =
        assertNotNull(node.visibleAnnotations?.singleOrNull { it.desc == "Lkotlin/Metadata;" })
}
