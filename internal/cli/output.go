package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/yaml"
)

// Output formats accepted by -o.
const (
	OutputTable = ""
	OutputWide  = "wide"
	OutputJSON  = "json"
	OutputYAML  = "yaml"
	OutputName  = "name"
)

var outputFormats = []string{OutputWide, OutputJSON, OutputYAML, OutputName}

// AddOutputFlag registers -o on commands that render a list of objects.
func AddOutputFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVarP(target, "output", "o", OutputTable,
		fmt.Sprintf("Output format. One of: %s", strings.Join(outputFormats, "|")))

	_ = cmd.RegisterFlagCompletionFunc("output",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return outputFormats, cobra.ShellCompDirectiveNoFileComp
		})
}

func ValidateOutput(format string) error {
	switch format {
	case OutputTable, OutputWide, OutputJSON, OutputYAML, OutputName:
		return nil
	default:
		return fmt.Errorf("unsupported output format %q, expected one of: %s",
			format, strings.Join(outputFormats, "|"))
	}
}

// IsMachineOutput reports whether the format renders objects rather than a table.
func IsMachineOutput(format string) bool {
	return format == OutputJSON || format == OutputYAML
}

// PrintObject writes obj as JSON or YAML. Callers set TypeMeta first, since a
// typed object read through a client does not carry its own apiVersion/kind.
func PrintObject(w io.Writer, format string, obj runtime.Object) error {
	switch format {
	case OutputJSON:
		data, err := json.MarshalIndent(obj, "", "    ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", data)
		return err
	case OutputYAML:
		data, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	default:
		return fmt.Errorf("%w", ValidateOutput(format))
	}
}

// NewTabWriter matches kubectl's column spacing.
func NewTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 6, 4, 3, ' ', 0)
}

// Age renders a creation timestamp the way kubectl does.
func Age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

// Since renders an optional timestamp as an age, or a placeholder when unset.
func Since(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "<none>"
	}
	return duration.HumanDuration(time.Since(t.Time)) + " ago"
}

// FindCondition returns the named condition, or nil.
func FindCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// ConditionStatus renders a condition for a table cell. An absent condition
// reads as Unknown rather than blank, since the operator may not have looked
// at the object yet.
func ConditionStatus(conds []metav1.Condition, condType string) string {
	if c := FindCondition(conds, condType); c != nil {
		return string(c.Status)
	}
	return string(metav1.ConditionUnknown)
}

func yesNo(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// printer collects write errors so a block of output can be checked once
// rather than at every call. That keeps the rendering code readable while
// still surfacing a real failure — a closed pipe from `corg get | head`, say —
// instead of dropping it.
type printer struct {
	w   io.Writer
	err error
}

func newPrinter(w io.Writer) *printer { return &printer{w: w} }

// Write lets a printer wrap a tabwriter, so tabulated output is checked too.
func (p *printer) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n, err := p.w.Write(b)
	p.err = err
	return n, err
}

func (p *printer) print(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprint(p.w, a...)
	}
}

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *printer) println(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, a...)
	}
}

// Err reports the first write error, if any.
func (p *printer) Err() error { return p.err }

// flushTable flushes a tabwriter, preferring an earlier write error.
func flushTable(p *printer, tw *tabwriter.Writer) error {
	if err := p.Err(); err != nil {
		return err
	}
	return tw.Flush()
}
