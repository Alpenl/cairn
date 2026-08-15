#!/usr/bin/env python3
from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
FIXTURE = Path(__file__).with_name("identity-migration.json")


@dataclass
class MigrationState:
    source_revision: int
    target_revision: int
    active_revision: int
    queue_revisions: dict[str, int]
    queue_keys: dict[str, str]
    recent_revision: int | None
    todo_revision: int | None
    todo_operation_ids: list[str]
    fail_once: set[str] = field(default_factory=set)
    queue_writes: int = 0
    recent_writes: int = 0
    recent_attempts: int = 0
    todo_writes: int = 0
    network_sends: int = 0
    recent_receipt: bool = False
    todo_receipt: bool = False
    target_changed: bool = False
    complete: bool = False
    drain: bool = False

    def execute(self, queue_ids: list[str], has_recent: bool) -> None:
        if self.active_revision != self.target_revision:
            self.target_changed = True
            self.complete = False
            self.drain = False
            return

        if self.todo_operation_ids and not self.todo_receipt:
            if "todo" in self.fail_once:
                self.fail_once.remove("todo")
            else:
                old_ids = list(self.todo_operation_ids)
                self.todo_operation_ids = [
                    f"r{self.target_revision}:{operation_id}" for operation_id in old_ids
                ]
                assert self.todo_operation_ids != old_ids
                self.todo_revision = self.target_revision
                self.todo_receipt = True
                self.todo_writes += 1

        recent_ok = not has_recent or self.recent_receipt
        if has_recent:
            self.recent_attempts += 1
            if self.recent_receipt:
                recent_ok = True
            elif "recent" in self.fail_once:
                self.fail_once.remove("recent")
                recent_ok = False
            else:
                self.recent_revision = self.target_revision
                self.recent_receipt = True
                self.recent_writes += 1
                recent_ok = True

        migrated_queue = 0
        for queue_id in queue_ids:
            if self.queue_revisions[queue_id] == self.target_revision:
                migrated_queue += 1
                continue
            if "queue" in self.fail_once:
                self.fail_once.remove("queue")
                continue
            old_key = self.queue_keys[queue_id]
            self.queue_revisions[queue_id] = self.target_revision
            self.queue_keys[queue_id] = f"new:{self.target_revision}:{queue_id}"
            assert self.queue_keys[queue_id] != old_key
            self.queue_writes += 1
            migrated_queue += 1

        todo_ok = not self.todo_operation_ids or self.todo_receipt
        self.complete = migrated_queue == len(queue_ids) and recent_ok and todo_ok
        self.drain = self.complete


def run_scenario(scenario: dict) -> None:
    source = scenario["source_revision"]
    target = scenario["target_revision"]
    queue_ids = list(scenario.get("queue_ids", []))
    foreign_ids = list(scenario.get("foreign_queue_ids", []))
    state = MigrationState(
        source_revision=source,
        target_revision=target,
        active_revision=scenario["active_revision_at_execute"],
        queue_revisions={
            **{queue_id: source for queue_id in queue_ids},
            **{queue_id: 2 for queue_id in foreign_ids},
        },
        queue_keys={
            queue_id: f"old:{queue_id}" for queue_id in queue_ids + foreign_ids
        },
        recent_revision=source if scenario.get("has_recent") else None,
        todo_revision=source if scenario.get("todo_operation_ids") else None,
        todo_operation_ids=list(scenario.get("todo_operation_ids", [])),
        fail_once=set(scenario.get("fail_once", [])),
    )

    prepared = {
        "queue": dict(state.queue_revisions),
        "keys": dict(state.queue_keys),
        "recent": state.recent_revision,
        "todo": list(state.todo_operation_ids),
    }
    actions = scenario["actions"]
    assert actions[0] == "prepare"
    assert prepared == {
        "queue": state.queue_revisions,
        "keys": state.queue_keys,
        "recent": state.recent_revision,
        "todo": state.todo_operation_ids,
    }, f"{scenario['name']}: prepare mutated state"

    if "cancel" not in actions:
        state.execute(queue_ids, scenario.get("has_recent", False))
    if "retry" in actions:
        state.execute(queue_ids, scenario.get("has_recent", False))

    expected = scenario["expected"]
    actual = {
        "complete": state.complete,
        "target_changed": state.target_changed,
        "queue_writes": state.queue_writes,
        "recent_writes": state.recent_writes,
        "todo_writes": state.todo_writes,
        "network_sends": state.network_sends,
        "drain": state.drain,
    }
    if "recent_attempts" in expected:
        actual["recent_attempts"] = state.recent_attempts
    if "foreign_queue_revision" in expected:
        actual["foreign_queue_revision"] = state.queue_revisions[foreign_ids[0]]
    assert actual == expected, f"{scenario['name']}: {actual} != {expected}"

    if state.complete:
        for queue_id in queue_ids:
            assert state.queue_revisions[queue_id] == target
            assert state.queue_keys[queue_id].startswith("new:")
        if scenario.get("has_recent"):
            assert state.recent_revision == target
        if scenario.get("todo_operation_ids"):
            assert state.todo_revision == target
            assert all(operation_id.startswith(f"r{target}:") for operation_id in state.todo_operation_ids)


def validate_swift_wiring() -> None:
    content = (ROOT / "ios/WebTagShare/App/ContentView.swift").read_text()
    core = (ROOT / "ios/WebTagShare/Shared/WebTagShareCore.swift").read_text()
    todo = (ROOT / "ios/WebTagShare/App/CompanionTodo.swift").read_text()
    tests = (ROOT / "ios/WebTagShareTests/WebTagShareTests.swift").read_text()
    required = {
        "ContentView.swift": [
            "IdentityMigrationOrchestrator",
            "cancelIdentityMigration()",
            "recentMigrationCandidates",
            "todoMigrationCandidates",
            "todoReadFailed",
            "withActiveTargetFence",
            "if result.isComplete { drain() }",
        ],
        "WebTagShareCore.swift": [
            "from source: ActivationIdentity",
            "to target: ActivationIdentity",
            "migrated_to_revision",
            "recentMigrationCandidates",
            "BEGIN IMMEDIATE;",
            "identity_migration_queue_receipts",
            "leaseOwner == nil",
            "withActiveTargetFence",
            "lease_owner, lease_expires_at FROM queue_entries WHERE id=?",
        ],
        "CompanionTodo.swift": [
            "migratedToRevision",
            "migrationCandidates(to target: ActivationIdentity)",
            "UUID().uuidString.lowercased()",
            "leaseOwner: nil",
            "nextAttemptAt: nil",
            "currentVersion = 3",
            "readableVersions = 1...currentVersion",
            "leaseExpiresAt.map({ $0 > now }) == true",
            "recordCommit()",
        ],
        "WebTagShareTests.swift": [
            "testProductMigrationPlanMovesQueueRecentAndTodoOnceAfterConfirmation",
            "testProductMigrationSelectsOnlyOriginalARevisionAfterAThenBThenA",
            "testProductMigrationCancellationStaleTargetAndPartialRetryAreWriteSafe",
            "testProductMigrationKeepsLiveQueueLeaseFrozenUntilClaimResolution",
            "testProductMigrationContinuesQueueAndRecentWhenTodoStoreIsUnavailable",
            "testTodoMigrationFenceBlocksConcurrentActivationUntilFileCommitReturns",
        ],
    }
    sources = {
        "ContentView.swift": content,
        "WebTagShareCore.swift": core,
        "CompanionTodo.swift": todo,
        "WebTagShareTests.swift": tests,
    }
    for name, tokens in required.items():
        missing = [token for token in tokens if token not in sources[name]]
        assert not missing, f"{name}: missing source contracts {missing}"


def main() -> None:
    fixture = json.loads(FIXTURE.read_text())
    assert fixture["schema_version"] == 1
    scenarios = fixture["scenarios"]
    assert len(scenarios) == 5
    for scenario in scenarios:
        run_scenario(scenario)
    validate_swift_wiring()
    print(f"validated {len(scenarios)} iOS identity migration scenarios and Swift wiring")


if __name__ == "__main__":
    main()
