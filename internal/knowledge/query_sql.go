package knowledge

import (
	"fmt"
	"strings"

	"github.com/clems4ever/go-graphkb/internal/query"
)

type SQLQueryTranslator struct {
	QueryGraph QueryGraph
}

func NewSQLQueryTranslator() *SQLQueryTranslator {
	return &SQLQueryTranslator{QueryGraph: NewQueryGraph()}
}

type AndOrExpression struct {
	And        bool // true for And and false for Or
	Children   []AndOrExpression
	Expression string
}

type Projection struct {
	Alias          string
	ExpressionType ExpressionType
}

type SQLTranslation struct {
	Query           string
	ProjectionTypes []Projection
}

func BuildAndOrExpression(tree AndOrExpression) (string, error) {
	if tree.Expression != "" {
		return tree.Expression, nil
	} else if tree.And {
		exprs := make([]string, 0)
		for i := range tree.Children {
			expr, err := BuildAndOrExpression(tree.Children[i])
			if err != nil {
				return "", err
			}
			if expr != "" {
				exprs = append(exprs, expr)
			}
		}
		if len(exprs) > 1 {
			return fmt.Sprintf("(%s)", strings.Join(exprs, " AND ")), nil
		}
		return strings.Join(exprs, " AND "), nil
	} else if !tree.And {
		exprs := make([]string, 0)
		for i := range tree.Children {
			expr, err := BuildAndOrExpression(tree.Children[i])
			if err != nil {
				return "", err
			}

			if expr != "" {
				exprs = append(exprs, expr)
			}
		}
		if len(exprs) > 1 {
			return fmt.Sprintf("(%s)", strings.Join(exprs, " OR ")), nil
		}
		return strings.Join(exprs, " OR "), nil
	}
	return "", nil
}

func CrossProductExpressions(and1 []AndOrExpression, and2 []AndOrExpression) []AndOrExpression {
	outExpr := []AndOrExpression{}
	for i := range and1 {
		for j := range and2 {
			children := AndOrExpression{
				And:      true,
				Children: []AndOrExpression{and1[i], and2[j]},
			}
			outExpr = append(outExpr, children)
		}
	}
	return outExpr
}

// UnwindOrExpressions in order to transform query with or relations into a union
// query which is more performant, an AndOrExpression is transformed into a list of AndExpressions
func UnwindOrExpressions(tree AndOrExpression) ([]AndOrExpression, error) {
	if tree.Expression != "" {
		child := AndOrExpression{Children: []AndOrExpression{tree}, And: true}
		return []AndOrExpression{child}, nil
	} else if !tree.And {
		exprs := []AndOrExpression{}
		for i := range tree.Children {
			nestedExpressions, err := UnwindOrExpressions(tree.Children[i])
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, nestedExpressions...)
		}
		return exprs, nil
	} else if tree.And {
		exprs := []AndOrExpression{}
		for i := range tree.Children {
			expr, err := UnwindOrExpressions(tree.Children[i])
			if err != nil {
				return nil, err
			}

			if len(exprs) == 0 {
				exprs = append(exprs, expr...)
			} else {
				exprs = CrossProductExpressions(exprs, expr)
			}
		}
		return exprs, nil
	}
	return nil, fmt.Errorf("Unable to detect kind of node")
}

func (sqt *SQLQueryTranslator) buildSQLSelect(
	distinct bool, projections []string, projectionTypes []Projection, fromTables []string,
	whereExpressions AndOrExpression, groupBy []int, limit int, offset int) (string, error) {
	var sqlQuery string

	andExpressions, err := UnwindOrExpressions(whereExpressions)
	if err != nil {
		return "", err
	}

	if len(andExpressions) > 1 {
		singleQueries := []string{}
		for _, where := range andExpressions {
			singleQuery, err := sqt.buildSingleSQLSelect(false, projections, fromTables, where, nil, 0, 0)
			if err != nil {
				return "", err
			}
			singleQueries = append(singleQueries, fmt.Sprintf("(%s)", singleQuery))
		}
		if distinct {
			sqlQuery = strings.Join(singleQueries, "\nUNION\n")
		} else {
			sqlQuery = strings.Join(singleQueries, "\nUNION ALL\n")
		}

		if len(groupBy) > 0 {
			groupByProjections := []string{}
			for i := range groupBy {
				groupByProjections = append(groupByProjections, projections[groupBy[i]])
			}

			sqlQuery = fmt.Sprintf("SELECT %s FROM\n(%s)\nGROUP BY %s",
				strings.Join(projections, ", "), sqlQuery, strings.Join(groupByProjections, ","))
		}

		if limit > 0 {
			sqlQuery += fmt.Sprintf("\nLIMIT %d", limit)
		}

		if offset > 0 {
			sqlQuery += fmt.Sprintf("\nOFFSET %d", offset)
		}

	} else {
		and := AndOrExpression{And: true, Children: andExpressions}
		singleQuery, err := sqt.buildSingleSQLSelect(distinct, projections, fromTables, and, groupBy, limit, offset)
		if err != nil {
			return "", err
		}
		sqlQuery = singleQuery
	}

	return sqlQuery, nil
}

func (sqt *SQLQueryTranslator) buildSingleSQLSelect(
	distinct bool, projections []string, fromTables []string,
	whereExpressions AndOrExpression, groupBy []int, limit int, offset int) (string, error) {

	projectionsStr := ""
	if distinct {
		projectionsStr += "DISTINCT "
	}
	projectionsStr += strings.Join(projections, ", ")
	fromTablesStr := strings.Join(fromTables, ", ")

	sqlQuery := fmt.Sprintf("SELECT %s FROM %s", projectionsStr, fromTablesStr)

	whereExprStr, err := BuildAndOrExpression(whereExpressions)
	if err != nil {
		return "", err
	}

	if whereExprStr != "" {
		sqlQuery += fmt.Sprintf("\nWHERE %s", whereExprStr)
	}

	if len(groupBy) > 0 {
		groupByProjection := make([]string, len(groupBy))
		for i := range groupBy {
			groupByProjection[i] = projections[groupBy[i]]
		}
		sqlQuery += fmt.Sprintf("\nGROUP BY %s", strings.Join(groupByProjection, ", "))
	}

	if limit > 0 {
		sqlQuery += fmt.Sprintf("\nLIMIT %d", limit)
	}

	if offset > 0 {
		sqlQuery += fmt.Sprintf("\nOFFSET %d", offset)
	}

	return sqlQuery, nil
}

func (sqt *SQLQueryTranslator) Translate(query *query.QueryCypher) (*SQLTranslation, error) {
	andExpressions := AndOrExpression{And: true}
	constrainedNodes := make(map[int]bool)

	filterExpressions := AndOrExpression{And: true}
	for _, x := range query.QuerySinglePartQuery.QueryMatches {
		for _, y := range x.PatternElements {
			_, i1, err := sqt.QueryGraph.PushNode(y.QueryNodePattern)
			if err != nil {
				return nil, err
			}

			for _, z := range y.QueryPatternElementChains {
				_, i2, err := sqt.QueryGraph.PushNode(z.QueryNodePattern)
				if err != nil {
					return nil, err
				}

				_, _, err = sqt.QueryGraph.PushRelation(z.RelationshipPattern, i1, i2)
				if err != nil {
					return nil, err
				}
				i1 = i2
			}
		}

		if x.Where != nil {
			whereVisitor := QueryWhereVisitor{}
			whereExpression, err := whereVisitor.ParseExpression(x.Where, &sqt.QueryGraph)
			if err != nil {
				return nil, err
			}
			for _, v := range whereVisitor.Variables {
				typeAndIndex, err := sqt.QueryGraph.FindVariable(v)
				if err != nil {
					return nil, err
				}
				constrainedNodes[typeAndIndex.Index] = true
			}
			filterExpressions.Children = append(filterExpressions.Children,
				AndOrExpression{Expression: whereExpression})
		}
	}

	projections := make([]string, 0)
	projectionTypes := make([]Projection, 0)
	from := make([]string, 0)

	unaggregatedProjectionItems := []int{}
	aggregationRequired := false

	for i, p := range query.QuerySinglePartQuery.ProjectionBody.ProjectionItems {
		projectionVisitor := ProjectionVisitor{QueryGraph: &sqt.QueryGraph}
		err := projectionVisitor.ParseExpression(&p.Expression)
		if err != nil {
			return nil, err
		}

		projection, err := NewExpressionBuilder(&sqt.QueryGraph).Build(&p.Expression)
		if err != nil {
			return nil, err
		}

		if !projectionVisitor.Aggregation {
			unaggregatedProjectionItems = append(unaggregatedProjectionItems, i)
		} else {
			aggregationRequired = true
		}

		projections = append(projections, projection)
		projectionTypes = append(projectionTypes, Projection{
			Alias:          p.Alias,
			ExpressionType: projectionVisitor.ExpressionType,
		})
	}

	if !aggregationRequired {
		unaggregatedProjectionItems = nil
	}

	for i, n := range sqt.QueryGraph.Nodes {
		alias := fmt.Sprintf("a%d", i)
		from = append(from, fmt.Sprintf("assets %s", alias))

		typesConstraints := AndOrExpression{And: false}
		for _, label := range n.Labels {
			typesConstraints.Children = append(typesConstraints.Children, AndOrExpression{
				Expression: fmt.Sprintf("%s.type = '%s'", alias, label),
			})
		}

		if len(typesConstraints.Children) > 0 {
			// Append assets constraints
			andExpressions.Children = append(andExpressions.Children, typesConstraints)
		}
	}
	for i, r := range sqt.QueryGraph.Relations {
		alias := fmt.Sprintf("r%d", i)
		from = append(from, fmt.Sprintf("relations %s", alias))

		typesConstraints := AndOrExpression{And: false}
		for _, label := range r.Labels {
			typesConstraints.Children = append(typesConstraints.Children, AndOrExpression{
				Expression: fmt.Sprintf("%s.type = '%s'", alias, label),
			})
		}
		if len(typesConstraints.Children) > 0 {
			andExpressions.Children = append(andExpressions.Children, typesConstraints)
		}

		out := AndOrExpression{
			And: true,
			Children: []AndOrExpression{
				AndOrExpression{
					Expression: fmt.Sprintf("%s.from_id = a%d.id", alias, r.LeftIdx),
				},
				AndOrExpression{
					Expression: fmt.Sprintf("%s.to_id = a%d.id", alias, r.RightIdx),
				},
			},
		}

		in := AndOrExpression{
			And: true,
			Children: []AndOrExpression{
				AndOrExpression{
					Expression: fmt.Sprintf("%s.from_id = a%d.id", alias, r.RightIdx),
				},
				AndOrExpression{
					Expression: fmt.Sprintf("%s.to_id = a%d.id", alias, r.LeftIdx),
				},
			},
		}

		if r.Direction == Right {
			andExpressions.Children = append(andExpressions.Children, out)
		} else if r.Direction == Left {
			andExpressions.Children = append(andExpressions.Children, in)
		} else if r.Direction == Either {
			oneDirectionOptimization := false
			// Optimization: in this case, finding in any direction is sufficient.
			if len(sqt.QueryGraph.Relations) == 1 {
				nodesConstrained := false
				for idx := range constrainedNodes {
					if idx == r.LeftIdx {
						nodesConstrained = true
					}
					if idx == r.RightIdx {
						nodesConstrained = true
					}
				}
				if !nodesConstrained {
					n, err := sqt.QueryGraph.FindNode(r.LeftIdx)
					if err != nil {
						return nil, err
					}
					if len(n.Labels) > 0 {
						nodesConstrained = true
					}

					n, err = sqt.QueryGraph.FindNode(r.RightIdx)
					if err != nil {
						return nil, err
					}
					if len(n.Labels) > 0 {
						nodesConstrained = true
					}
				}
				oneDirectionOptimization = !nodesConstrained
			}

			if oneDirectionOptimization {
				andExpressions.Children = append(andExpressions.Children, out)
			} else {
				orExpression := AndOrExpression{
					And:      false,
					Children: []AndOrExpression{out, in},
				}
				andExpressions.Children = append(andExpressions.Children, orExpression)
			}
		}
	}

	limit := 0
	if query.QuerySinglePartQuery.ProjectionBody.Limit != nil {
		limitVisitor := QueryLimitVisitor{}
		err := limitVisitor.ParseExpression(
			query.QuerySinglePartQuery.ProjectionBody.Limit)
		if err != nil {
			return nil, err
		}
		limit = int(limitVisitor.Limit)
	}

	offset := 0
	if query.QuerySinglePartQuery.ProjectionBody.Skip != nil {
		if limit == 0 {
			return nil, fmt.Errorf("SKIP must be used in combination with limit")
		}
		skipVisitor := QuerySkipVisitor{}
		err := skipVisitor.ParseExpression(
			query.QuerySinglePartQuery.ProjectionBody.Skip)
		if err != nil {
			return nil, err
		}
		offset = int(skipVisitor.Skip)
	}

	if len(filterExpressions.Children) > 0 {
		andExpressions.Children = append(andExpressions.Children, filterExpressions)
	}

	sqlQuery, err := sqt.buildSQLSelect(
		query.QuerySinglePartQuery.ProjectionBody.Distinct,
		projections,
		projectionTypes,
		from,
		andExpressions,
		unaggregatedProjectionItems,
		limit, offset)

	if err != nil {
		return nil, err
	}

	return &SQLTranslation{
		Query:           sqlQuery,
		ProjectionTypes: projectionTypes,
	}, nil
}
