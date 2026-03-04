// weekly_review: Queries the database for a 7-day activity summary across
// memories, goals, work items, routines, and personality evolution.

declare function call_tool(name: string, args: Record<string, any>): string;

interface QueryResult {
  label: string;
  query: string;
}

const queries: QueryResult[] = [
  {
    label: "Memory Activity (7 days)",
    query:
      "Count memories by memory_type from the last 7 days. " +
      "Also return the top 5 highest-salience summaries (excluding goal and identity types).",
  },
  {
    label: "Active Goals",
    query:
      "List all memories where memory_type='goal' and superseded_by IS NULL and salience >= 5, " +
      "created in last 30 days. For each, count work_items from the last 7 days whose directive " +
      "contains the goal text (ILIKE match), split by status.",
  },
  {
    label: "Work Items (7 days)",
    query:
      "Count work_items from the last 7 days grouped by status. " +
      "Also list the top 5 most recently completed directives.",
  },
  {
    label: "Routine Health",
    query:
      "Count work_items where type='routine' from the last 7 days, grouped by directive. " +
      "Show total runs and completed runs.",
  },
  {
    label: "Personality Evolution",
    query:
      "List personality_traits where updated_at is within the last 7 days, " +
      "showing category, trait_key, and trait_value.",
  },
];

(function main() {
  const sections: string[] = [];

  for (const q of queries) {
    let result: string;
    try {
      result = call_tool("ask_database", { question: q.query });
    } catch (e) {
      result = "(query failed: " + e + ")";
    }
    sections.push("## " + q.label + "\n" + result);
  }

  return sections.join("\n\n");
})();
