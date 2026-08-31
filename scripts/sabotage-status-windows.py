#!/usr/bin/env python3
"""Sabotage cases for the three HTTP status-code windows llm-bridge-hermes decides on.

Card `3c18632a` — sweep the fleet's corpora for numeric boundaries no row
straddles. `config_method_test.go` is one of its 101 paths, and the five bounds
the census recorded against it are the three windows below plus `len(os.Args)`.

The card's thesis is that a suite can name a numeric boundary, exercise the
mechanism that boundary guards on every row, and still have every row land on
the same side — at which point the boundary is documented, exercised and
unpinned all at once. This repo holds three such windows:

    client.go:193       resp.StatusCode < 200 || resp.StatusCode >= 300
    dashboard.go:47     resp.StatusCode < 200 || resp.StatusCode >= 300   (the SAME
                        window, second implementation)
    handler.go:612      (statusCode >= 500 && statusCode < 600) || statusCode == 429

Before this file, every fixture in the suite that reaches those windows sends
either 200 or 500. `TestDoJSON_Success` / `TestDoJSON_NilOut` sit at 200,
`TestDoJSON_Error` and `TestDashboardListSessions_Error` at 500,
`TestSendResponses_HTTPError` at 429. Not one of the four window edges — 199,
200, 299, 300 — has a fixture on both sides, and NOTHING asserts the
`Retryable` flag for an API error at all, so all three of 500, 600 and the 429
special case are free.

⭐ `client.go:193` and `dashboard.go:47` are the 181st pass's rule on this card
arriving in llm-bridge-hermes: **split the population by IMPLEMENTATION, not by
repo or by file.** `hermesClient.doJSON` and `dashboardClient.doJSON` each carry
their own copy of "2xx is success". The two blocks are byte-identical for three
lines and diverge only at the `Message` format string, so every needle here has
to run down to that line — an ambiguous needle is evidence about the code, not a
nuisance to lengthen past.

## Widen AND narrow, never a single step

A window is free anywhere between the largest fixture inside it and the smallest
outside it, and a one-directional probe measures only one end of that range.

## What a case here is NOT allowed to be

⛔ A mutation that cannot change an observable answer is not a case. Every row
below moves an edge past a status code a fixture can actually carry, and each
was pre-verified one at a time against the whole suite before the scored run.

Run with --diffs at least once and read each applied edit against its label. A
row prints the name it was given, not the edit it made.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

# One package: every .go file in this repo is `package main` at the root.
PACKAGES = ["./..."]

TARGETS = [REPO / "client.go", REPO / "dashboard.go", REPO / "handler.go"]

# No fixture in this repo guards on "the request never reaches the window":
# every case below moves an edge, and a moved edge still returns an answer.
# Declared empty on purpose rather than omitted, so the next reader knows the
# question was asked. See sabotage.classify_caught().
GUARD_MARKERS = ()


def window(flavour, old, new):
    """A needle for one of the two byte-identical doJSON windows.

    `flavour` is the word in the Message format string, and it is the ONLY thing
    separating the hermes copy from the dashboard copy. Running down to that
    line is what makes the needle unique; a three-line needle matches both files
    and the engine refuses it.
    """
    head = ("\tif resp.StatusCode < 200 || resp.StatusCode >= 300 {\n"
            "\t\tb, _ := io.ReadAll(resp.Body)\n"
            "\t\treturn &apiError{\n"
            "\t\t\tStatusCode: resp.StatusCode,\n"
            "\t\t\tMessage:    fmt.Sprintf(\"%s API error: " % flavour)
    return [(head, head.replace(old, new, 1))]


CASES = [
    # ---- the hermes client's success window, copy 1 of 2 -------------------
    Case("hermes doJSON: lower edge 200 -> 100, so a 1xx counts as success",
         window("hermes", "< 200", "< 100")),
    Case("hermes doJSON: lower edge 200 -> 201, so a plain 200 becomes an error",
         window("hermes", "< 200", "< 201")),
    Case("hermes doJSON: upper edge 300 -> 400, so a 3xx counts as success",
         window("hermes", ">= 300", ">= 400")),
    Case("hermes doJSON: upper edge 300 -> 299, so a 299 becomes an error",
         window("hermes", ">= 300", ">= 299")),

    # ---- the dashboard client's success window, copy 2 of 2 ----------------
    #
    # The same four edges again. A row that pins one copy says nothing about the
    # other, and a reader who greps `resp.StatusCode < 200` and finds a test
    # near the first copy will believe both are held.
    Case("dashboard doJSON: lower edge 200 -> 100, so a 1xx counts as success",
         window("dashboard", "< 200", "< 100")),
    Case("dashboard doJSON: lower edge 200 -> 201, so a plain 200 becomes an error",
         window("dashboard", "< 200", "< 201")),
    Case("dashboard doJSON: upper edge 300 -> 400, so a 3xx counts as success",
         window("dashboard", ">= 300", ">= 400")),
    Case("dashboard doJSON: upper edge 300 -> 299, so a 299 becomes an error",
         window("dashboard", ">= 300", ">= 299")),

    # ---- the retryable window on an API error -----------------------------
    #
    # Three axes on one line: where the 5xx band starts, where it ends, and the
    # 429 special case that sits outside it. The comment above the line says
    # "5xx errors and 429 are typically retryable" — a claim in prose that no
    # assertion holds.
    Case("retryable: 5xx band starts at 501, so a 500 stops being retryable",
         [("retryable = (statusCode >= 500 && statusCode < 600)",
           "retryable = (statusCode >= 501 && statusCode < 600)")]),
    Case("retryable: 5xx band starts at 429, so every 4xx from 429 up is retryable",
         [("retryable = (statusCode >= 500 && statusCode < 600)",
           "retryable = (statusCode >= 429 && statusCode < 600)")]),
    Case("retryable: 5xx band ends at 599, so a 599 stops being retryable",
         [("retryable = (statusCode >= 500 && statusCode < 600)",
           "retryable = (statusCode >= 500 && statusCode < 599)")]),
    Case("retryable: 5xx band ends at 700, so a 600 becomes retryable",
         [("retryable = (statusCode >= 500 && statusCode < 600)",
           "retryable = (statusCode >= 500 && statusCode < 700)")]),
    Case("retryable: the rate-limit special case is 428, not 429",
         [("statusCode < 600) || statusCode == 429",
           "statusCode < 600) || statusCode == 428")]),

    # ---- three rows added so that no FIXTURE is redundant ------------------
    #
    # Card `2b2553aa`'s rule: a channel that no case needs ALONE is a channel
    # the case list has not measured. Counting how many cases exercise a fixture
    # together says nothing; ask which case fails when that fixture alone is
    # gone. Scored against the five rows above, three of the seven retryable
    # fixtures were redundant — 499 only ever reddened alongside 430, and 429
    # and 428 only ever reddened together. These three aim at one fixture each.
    Case("retryable: 5xx band starts at 499, so a 499 becomes retryable (aims at 499 alone)",
         [("retryable = (statusCode >= 500 && statusCode < 600)",
           "retryable = (statusCode >= 499 && statusCode < 600)")]),
    Case("retryable: the rate-limit special case is dropped (aims at 429 alone)",
         [("statusCode < 600) || statusCode == 429",
           "statusCode < 600) || statusCode == 0")]),
    Case("retryable: 428 joins the rate-limit special case (aims at 428 alone)",
         [("statusCode < 600) || statusCode == 429",
           "statusCode < 600) || statusCode == 429 || statusCode == 428")]),

    # ---- controls ---------------------------------------------------------
    #
    # Two, not one. A lone UNNOTICED control reads exactly like a broken
    # harness, and it takes a second control coming back CAUGHT to show the
    # instrument is fine and the first row is a finding.
    Case("CONTROL known-positive: sendResponses treats 429, not 200, as the good status",
         [("\tif resp.StatusCode != http.StatusOK {\n"
           "\t\tb, _ := io.ReadAll(resp.Body)\n"
           "\t\treturn nil, &apiError{",
           "\tif resp.StatusCode != http.StatusTooManyRequests {\n"
           "\t\tb, _ := io.ReadAll(resp.Body)\n"
           "\t\treturn nil, &apiError{")]),
    Case("CONTROL known-negative: the hermes window is written the other way round",
         window("hermes", "resp.StatusCode < 200 || resp.StatusCode >= 300",
                "200 > resp.StatusCode || 300 <= resp.StatusCode"),
         expected_unnoticed="`a < 200 || a >= 300` and `200 > a || 300 <= a` are the "
                            "same two comparisons written in the other order. Nothing "
                            "can tell them apart, and a run that reddens here says the "
                            "harness is mutating something other than what its label "
                            "claims. FALSIFY: any red run on this row."),
]


def main():
    print("targets: %s" % ", ".join(str(t.relative_to(REPO)) for t in TARGETS))
    return score(TARGETS, PACKAGES, CASES, guard_markers=GUARD_MARKERS)


if __name__ == "__main__":
    sys.exit(main())
