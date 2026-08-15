package com.alpenl.webtag.share

import com.alpenl.webtag.share.data.TodoOutboxEntity
import com.alpenl.webtag.share.data.TodoOutboxEnvelope
import com.alpenl.webtag.share.data.cacheV2Aad
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Test

class TodoOutboxEnvelopeTest {
    @Test
    fun canonicalV3AadHasAFixedLengthPrefixedVector() {
        assertEquals(
            """
            webtag.todo-outbox-envelope
            version=1:3
            cryptoVersion=1:1
            operationId=4:op-1
            apiOrigin=19:https://example.org
            clientDataNamespace=3:abc
            activationRevision=1:7
            kind=5:patch
            targetTodoId=6:todo-1
            state=10:retry_wait
            attemptCount=1:2
            nextAttemptAt=4:9000
            lastErrorKind=8:HTTP_5XX
            leaseOwner=1:~
            leaseExpiresAt=1:~
            createdAt=4:1000
            updatedAt=4:1001
            quarantinedLegacyState=1:~
            """.trimIndent() + "\n",
            TodoOutboxEnvelope.canonicalAad(entity()),
        )
    }

    @Test
    fun everyAuthenticatedStateMachineFieldChangesTheAad() {
        val original = entity()
        val aad = TodoOutboxEnvelope.canonicalAad(original)
        listOf(
            original.copy(operationId = "op-2"),
            original.copy(cryptoVersion = 2),
            original.copy(apiOrigin = "https://other.example"),
            original.copy(clientDataNamespace = "other"),
            original.copy(activationRevision = 8),
            original.copy(kind = "delete"),
            original.copy(targetTodoId = "todo-2"),
            original.copy(state = "blocked_auth"),
            original.copy(attemptCount = 3),
            original.copy(nextAttemptAt = null),
            original.copy(lastErrorKind = null),
            original.copy(leaseOwner = "worker"),
            original.copy(leaseExpiresAt = 10_000),
            original.copy(createdAt = 1001),
            original.copy(updatedAt = 1002),
            original.copy(quarantinedLegacyState = "pending"),
        ).forEach { changed ->
            assertNotEquals(changed.toString(), aad, TodoOutboxEnvelope.canonicalAad(changed))
        }
    }

    @Test
    fun cacheV2AadHasARevisionBoundFixedVector() {
        assertEquals(
            """
            webtag.todo-cache-envelope
            version=1:2
            todoId=6:todo-1
            apiOrigin=19:https://example.org
            clientDataNamespace=3:abc
            activationRevision=1:7
            """.trimIndent() + "\n",
            cacheV2Aad("todo-1", "https://example.org", "abc", 7),
        )
    }

    private fun entity() = TodoOutboxEntity(
        operationId = "op-1",
        apiOrigin = "https://example.org",
        clientDataNamespace = "abc",
        targetTodoId = "todo-1",
        kind = "patch",
        payloadCiphertext = byteArrayOf(1),
        payloadNonce = byteArrayOf(2),
        cryptoVersion = 1,
        state = "retry_wait",
        attemptCount = 2,
        nextAttemptAt = 9000,
        lastErrorKind = "HTTP_5XX",
        leaseOwner = null,
        leaseExpiresAt = null,
        createdAt = 1000,
        updatedAt = 1001,
        activationRevision = 7,
        envelopeVersion = 3,
    )
}
