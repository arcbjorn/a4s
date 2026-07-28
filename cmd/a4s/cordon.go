package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/arcbjorn/a4s/control"
)

// cordon takes a node out of service, or returns it, through a running server.
//
// This is the planned-maintenance path. The remediation agent already cordons a
// node it has measured as failing, which handles the unplanned case; nothing
// observes a reason to stop scheduling onto a machine an operator is merely
// about to open up.
//
// It reports what is still running on the node rather than only confirming the
// cordon, because "it is out of the rotation" is not the question an operator
// asks next. They ask what still has to move, and which of it holds data.
func cordon(args []string) error {
	flags := flag.NewFlagSet("cordon", flag.ContinueOnError)
	connection := registerClientFlags(flags)
	node := flags.String("node", "", "node to take out of service")
	reason := flags.String("reason", "", "why the node is being cordoned")
	undo := flags.Bool("undo", false, "return the node to service instead")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *node == "" {
		return fmt.Errorf("node is required")
	}
	client, err := connection.client()
	if err != nil {
		return err
	}

	if *undo {
		if _, err := client.do(http.MethodPost, "/v1/nodes/"+*node+"/uncordon", nil); err != nil {
			return err
		}
		fmt.Printf("node %s is schedulable again\n", *node)
		return nil
	}

	var body any
	if *reason != "" {
		body = map[string]string{"reason": *reason}
	}
	answer, err := client.do(http.MethodPost, "/v1/nodes/"+*node+"/cordon", body)
	if err != nil {
		return err
	}
	if *jsonOutput {
		fmt.Print(string(answer))
		return nil
	}

	var evacuation control.NodeEvacuation
	if err := json.Unmarshal(answer, &evacuation); err != nil {
		return fmt.Errorf("decode evacuation: %w", err)
	}
	fmt.Printf("node %s will accept no new allocations\n", *node)
	if evacuation.Empty() {
		fmt.Println("nothing is running on it; it is drained")
		return nil
	}
	fmt.Printf("still running: %s\n", strings.Join(evacuation.Allocations, ", "))
	if len(evacuation.Stateful) > 0 {
		// Named separately because these are the ones an operator has to decide
		// about. Everything else the remediation agent will move on its own.
		fmt.Printf("holding durable data: %s\n", strings.Join(evacuation.Stateful, ", "))
		fmt.Println("these need a destroy-stateful approval before they can be moved")
	}
	return nil
}
