plugins {
    java
}

providers.gradleProperty("probeBuildDir").orNull?.let { layout.buildDirectory.set(file(it)) }

repositories { mavenCentral() }

dependencies {
    compileOnly("org.jetbrains.kotlin:kotlin-compiler-embeddable:2.3.20")
}

tasks.withType<JavaCompile>().configureEach {
    options.release.set(17)
}

tasks.jar {
    archiveBaseName.set("codegraph-kotlin-compiler-probe")
    archiveVersion.set("1.0.0-kotlin-2.3.20")
    manifest {
        attributes(
            "Implementation-Version" to "1.0.0",
            "CG35-Protocol-Version" to "1",
            "Kotlin-Compiler-Version" to "2.3.20",
        )
    }
}
