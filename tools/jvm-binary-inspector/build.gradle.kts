plugins {
    kotlin("jvm") version "2.3.20"
    application
}

group = "org.codegraph"
version = "1.0.0"

repositories { mavenCentral() }

dependencies {
    implementation("org.jetbrains.kotlin:kotlin-metadata-jvm:2.3.20")
    implementation("org.ow2.asm:asm:9.8")
    implementation("org.ow2.asm:asm-tree:9.8")
    testImplementation(kotlin("test"))
}

kotlin { jvmToolchain(17) }

application { mainClass.set("org.codegraph.jvm.binary.MainKt") }

tasks.test { useJUnitPlatform() }
