package org.codegraph.jvm.binary

import java.nio.file.Files
import java.nio.file.Path
import java.security.MessageDigest
import kotlin.system.exitProcess
import kotlin.Metadata
import kotlin.metadata.ClassKind
import kotlin.metadata.kind
import kotlin.metadata.MemberKind
import kotlin.metadata.isSuspend
import kotlin.metadata.visibility
import kotlin.metadata.Visibility
import kotlin.metadata.jvm.JvmMethodSignature
import kotlin.metadata.jvm.KotlinClassMetadata
import kotlin.metadata.jvm.signature
import org.objectweb.asm.ClassReader
import org.objectweb.asm.ClassVisitor
import org.objectweb.asm.FieldVisitor
import org.objectweb.asm.MethodVisitor
import org.objectweb.asm.Opcodes
import org.objectweb.asm.tree.AnnotationNode
import org.objectweb.asm.tree.ClassNode

private const val SCHEMA = "codegraph.jvm_binary_signature_evidence/v1"
private const val PROTOCOL_VERSION = 1
private const val PRODUCER = "codegraph-jvm-binary-inspector"
private const val PRODUCER_VERSION = "1.0.0"
private const val MAX_CLASS_BYTES = 16 * 1024 * 1024
private const val MAX_JSON_BYTES = 2 * 1024 * 1024
private const val MAX_METHODS = 4096
private const val MAX_METADATA_STRINGS = 100_000
private const val MAX_METADATA_STRING_CHARS = 1_000_000
private val SUPPORTED_METADATA_VERSION = listOf(2, 3, 0)

private data class MetadataHeader(
    val kind: Int,
    val metadataVersion: List<Int>,
    val bytecodeVersion: List<Int>,
    val data1: Array<String>,
    val data2: Array<String>,
    val extraString: String,
    val packageName: String,
    val extraInt: Int,
) {
    fun toMetadata(): Metadata = Metadata(
        kind,
        metadataVersion.toIntArray(),
        bytecodeVersion.toIntArray(),
        data1,
        data2,
        extraString,
        packageName,
        extraInt,
    )
}

internal data class VerifiedMethod(
    val kotlin_name: String,
    val jvm_name: String,
    val descriptor: String,
    val metadata_association: String = "unique",
    val binary_signature_state: String = "verified",
    val confidence: String = "exact",
)

internal data class MetadataFact(val state: String, val version: List<Int>)

internal data class TrustFact(
    val source_to_binary_association: String = "unknown",
    val current_gradle_task_selection: String = "unknown",
    val current_build_freshness: String = "unknown",
    val compiler_input_completeness: String = "unknown",
    val compiler_output_ownership: String = "unknown",
)

internal data class Result(
    val schema: String = SCHEMA,
    val protocol_version: Int = PROTOCOL_VERSION,
    val producer: Map<String, String> = mapOf("name" to PRODUCER, "version" to PRODUCER_VERSION),
    val artifact: Map<String, String>,
    val classfile: Map<String, String>,
    val kotlin_metadata: MetadataFact,
    val methods: List<VerifiedMethod>,
    val unsupported_declarations: Int,
    val unassociated_method_count: Int,
    val trust: TrustFact = TrustFact(),
)

private class InspectionException(message: String) : Exception(message)

fun main(args: Array<String>) {
    try {
        val input = parseArgs(args)
        val bytes = readBounded(input)
        println(encodeResult(inspect(bytes)))
    } catch (failure: InspectionException) {
        System.err.println(failure.message ?: "inspection failed")
        exitProcess(2)
    } catch (failure: Exception) {
        System.err.println("class-file inspection failed")
        exitProcess(2)
    }
}

private fun parseArgs(args: Array<String>): Path {
    if (args.size != 2 || args[0] != "--input" || args[1].isBlank()) {
        throw InspectionException("usage: inspector --input FILE")
    }
    return Path.of(args[1])
}

private fun readBounded(path: Path): ByteArray {
    try {
        val size = Files.size(path)
        if (size <= 0 || size > MAX_CLASS_BYTES) {
            throw InspectionException("class file is empty or exceeds the 16 MiB limit")
        }
        return Files.newInputStream(path).use { input ->
            val bytes = input.readNBytes(MAX_CLASS_BYTES + 1)
            if (bytes.size > MAX_CLASS_BYTES) {
                throw InspectionException("class file exceeds the 16 MiB limit")
            }
            bytes
        }
    } catch (failure: InspectionException) {
        throw failure
    } catch (failure: Exception) {
        throw InspectionException("cannot read staged class file")
    }
}

internal fun inspect(bytes: ByteArray): Result {
    if (bytes.isEmpty() || bytes.size > MAX_CLASS_BYTES) {
        throw InspectionException("class file is empty or exceeds the 16 MiB limit")
    }
    val hash = MessageDigest.getInstance("SHA-256").digest(bytes).joinToString("") { "%02x".format(it) }
    val node = ClassNode(Opcodes.ASM9)
    try {
        // First validate all class and Code structures without retaining bytecode
        // instructions. The second pass keeps only the bounded member inventory.
        ClassReader(bytes).accept(object : ClassVisitor(Opcodes.ASM9) {
            override fun visitField(
                access: Int,
                name: String,
                descriptor: String,
                signature: String?,
                value: Any?,
            ): FieldVisitor = object : FieldVisitor(Opcodes.ASM9) {}

            override fun visitMethod(
                access: Int,
                name: String,
                descriptor: String,
                signature: String?,
                exceptions: Array<out String>?,
            ): MethodVisitor = object : MethodVisitor(Opcodes.ASM9) {}
        }, ClassReader.SKIP_DEBUG or ClassReader.SKIP_FRAMES)
        ClassReader(bytes).accept(node, ClassReader.SKIP_CODE or ClassReader.SKIP_DEBUG or ClassReader.SKIP_FRAMES)
    } catch (failure: Exception) {
        throw InspectionException("malformed JVM class file")
    }

    val classMethods = node.methods
    if (classMethods.size > 65_535) throw InspectionException("class method count exceeds JVM limits")
    val metadataAnnotation = findMetadataAnnotation(node)
    if (metadataAnnotation == null) {
        return result(hash, node.name, MetadataFact("absent", emptyList()), emptyList(), 0, classMethods.size)
    }
    val header = parseHeader(metadataAnnotation)
    if (header.metadataVersion != SUPPORTED_METADATA_VERSION) {
        throw InspectionException("unsupported Kotlin metadata version ${header.metadataVersion.joinToString(".")}")
    }
    val metadata = try {
        KotlinClassMetadata.readStrict(header.toMetadata())
    } catch (failure: Exception) {
        throw InspectionException("Kotlin metadata is malformed or unsupported")
    }
    val classMetadata = metadata as? KotlinClassMetadata.Class
        ?: return result(hash, node.name, MetadataFact("unsupported_kind", header.metadataVersion), emptyList(), 0, classMethods.size)
    val kmClass = classMetadata.kmClass
    if (kmClass.functions.size > MAX_METHODS) {
        throw InspectionException("Kotlin declaration count exceeds the 4096 record limit")
    }
    if (
        kmClass.kind != ClassKind.CLASS ||
        kmClass.visibility != Visibility.PUBLIC ||
        (node.access and Opcodes.ACC_PUBLIC) == 0 ||
        (node.access and Opcodes.ACC_INTERFACE) != 0 ||
        node.name.contains('$')
    ) {
        return result(hash, node.name, MetadataFact("unsupported_class_shape", header.metadataVersion), emptyList(), kmClass.functions.size, classMethods.size)
    }
    if (kmClass.name != node.name) {
        throw InspectionException("Kotlin metadata owner does not match the classfile owner")
    }

    val matches = mutableListOf<VerifiedMethod>()
    val matchedJvmSignatures = mutableSetOf<Pair<String, String>>()
    var unsupported = 0
    for (function in kmClass.functions) {
        if (
            function.kind != MemberKind.DECLARATION ||
            function.visibility != Visibility.PUBLIC ||
            function.isSuspend
        ) {
            unsupported++
            continue
        }
        val signature = function.signature
        if (signature == null) {
            unsupported++
            continue
        }
        val candidates = classMethods.filter { it.name == signature.name && it.desc == signature.descriptor }
        if (candidates.size != 1) {
            throw InspectionException("Kotlin metadata method signature does not match exactly one classfile method")
        }
        val method = candidates.single()
        if (
            (method.access and Opcodes.ACC_PUBLIC) == 0 ||
            (method.access and (Opcodes.ACC_STATIC or Opcodes.ACC_SYNTHETIC or Opcodes.ACC_BRIDGE)) != 0
        ) {
            unsupported++
            continue
        }
        val signatureKey = signature.name to signature.descriptor
        if (!matchedJvmSignatures.add(signatureKey)) {
            throw InspectionException("ambiguous Kotlin metadata method association")
        }
        matches += VerifiedMethod(function.name, signature.name, signature.descriptor)
    }
    val ordered = matches.sortedWith(compareBy<VerifiedMethod> { it.jvm_name }.thenBy { it.descriptor })
    return result(
        hash,
        node.name,
        MetadataFact("supported", header.metadataVersion),
        ordered,
        unsupported,
        classMethods.size - matchedJvmSignatures.size,
    )
}

private fun result(
    hash: String,
    owner: String,
    metadata: MetadataFact,
    methods: List<VerifiedMethod>,
    unsupported: Int,
    unassociated: Int,
) = Result(
    artifact = mapOf("sha256" to hash, "state" to "exact_bytes"),
    classfile = mapOf("owner" to owner, "method_table_state" to "parsed"),
    kotlin_metadata = metadata,
    methods = methods,
    unsupported_declarations = unsupported,
    unassociated_method_count = unassociated,
)

private fun findMetadataAnnotation(node: ClassNode): AnnotationNode? {
    val all = node.visibleAnnotations.orEmpty().plus(node.invisibleAnnotations.orEmpty())
        .filter { it.desc == "Lkotlin/Metadata;" }
    if (all.size > 1) throw InspectionException("duplicate Kotlin metadata annotations")
    return all.singleOrNull()
}

private fun parseHeader(annotation: AnnotationNode): MetadataHeader {
    val values = annotation.values.orEmpty()
    if (values.size % 2 != 0) throw InspectionException("malformed Kotlin metadata annotation")
    val fields = mutableMapOf<String, Any?>()
    for (i in values.indices step 2) {
        val name = values[i] as? String ?: throw InspectionException("malformed Kotlin metadata annotation")
        if (fields.containsKey(name)) throw InspectionException("duplicate Kotlin metadata field")
        fields[name] = values[i + 1]
    }
    fun intField(field: String, default: Int): Int {
        if (!fields.containsKey(field)) return default
        return fields[field] as? Int ?: throw InspectionException("malformed Kotlin metadata $field")
    }
    fun stringField(field: String): String {
        if (!fields.containsKey(field)) return ""
        return fields[field] as? String ?: throw InspectionException("malformed Kotlin metadata $field")
    }
    fun intList(field: String): List<Int> {
        val raw = fields[field] ?: return emptyList()
        val values = raw as? List<*> ?: throw InspectionException("malformed Kotlin metadata $field")
        if (values.size > MAX_METADATA_STRINGS) throw InspectionException("Kotlin metadata record limit exceeded")
        return values.map { it as? Int ?: throw InspectionException("malformed Kotlin metadata $field") }
    }
    fun stringList(field: String): Array<String> {
        val raw = fields[field] ?: return emptyArray()
        val values = raw as? List<*> ?: throw InspectionException("malformed Kotlin metadata $field")
        if (values.size > MAX_METADATA_STRINGS) throw InspectionException("Kotlin metadata record limit exceeded")
        return values.map {
            val string = it as? String ?: throw InspectionException("malformed Kotlin metadata $field")
            if (string.length > MAX_METADATA_STRING_CHARS) throw InspectionException("Kotlin metadata string limit exceeded")
            string
        }.toTypedArray()
    }
    val kind = intField("k", 1)
    val metadataVersion = intList("mv")
    if (metadataVersion.size != 3) throw InspectionException("malformed Kotlin metadata version")
    val bytecodeVersion = intList("bv")
    val extraString = stringField("xs")
    val packageName = stringField("pn")
    val extraInt = intField("xi", 0)
    return MetadataHeader(kind, metadataVersion, bytecodeVersion, stringList("d1"), stringList("d2"), extraString, packageName, extraInt)
}

private fun encodeResult(result: Result): String = buildString {
    append('{')
    field("schema", result.schema)
    append(','); field("protocol_version", result.protocol_version)
    append(",\"producer\":{"); field("name", result.producer.getValue("name")); append(','); field("version", result.producer.getValue("version")); append('}')
    append(",\"artifact\":{"); field("sha256", result.artifact.getValue("sha256")); append(','); field("state", result.artifact.getValue("state")); append('}')
    append(",\"classfile\":{"); field("owner", result.classfile.getValue("owner")); append(','); field("method_table_state", result.classfile.getValue("method_table_state")); append('}')
    append(",\"kotlin_metadata\":{"); field("state", result.kotlin_metadata.state); append(",\"version\":[")
    append(result.kotlin_metadata.version.joinToString(",")); append("]}")
    append(",\"methods\":[")
    result.methods.forEachIndexed { index, method ->
        if (index > 0) append(',')
        append('{'); field("kotlin_name", method.kotlin_name); append(','); field("jvm_name", method.jvm_name)
        append(','); field("descriptor", method.descriptor); append(','); field("metadata_association", method.metadata_association)
        append(','); field("binary_signature_state", method.binary_signature_state); append(','); field("confidence", method.confidence); append('}')
    }
    append(']'); append(','); field("unsupported_declarations", result.unsupported_declarations)
    append(','); field("unassociated_method_count", result.unassociated_method_count)
    append(",\"trust\":{")
    field("source_to_binary_association", "unknown")
    append(','); field("current_gradle_task_selection", "unknown")
    append(','); field("current_build_freshness", "unknown")
    append(','); field("compiler_input_completeness", "unknown")
    append(','); field("compiler_output_ownership", "unknown")
    append("}}")
}.also {
    if (it.toByteArray(Charsets.UTF_8).size > MAX_JSON_BYTES) {
        throw InspectionException("JSON response exceeds the 2 MiB limit")
    }
}

private fun StringBuilder.field(name: String, value: String) {
    append(quote(name)); append(':'); append(quote(value))
}

private fun StringBuilder.field(name: String, value: Int) {
    append(quote(name)); append(':'); append(value)
}

private fun quote(value: String): String = buildString(value.length + 2) {
    append('"')
    for (char in value) {
        when (char) {
            '"' -> append("\\\"")
            '\\' -> append("\\\\")
            '\b' -> append("\\b")
            '\u000c' -> append("\\f")
            '\n' -> append("\\n")
            '\r' -> append("\\r")
            '\t' -> append("\\t")
            else -> if (char.code < 0x20) append("\\u%04x".format(char.code)) else append(char)
        }
    }
    append('"')
}
