use super::*;
use std::collections::BTreeSet;

fn codes() -> Vec<u32> {
    (1..=32).chain([50, 200]).collect()
}

// Independent unbounded BFS oracle: no factor distance/next or production repair.
fn distances(adjacency: &[Vec<u32>], source: u32, failed: Option<u32>) -> Vec<u32> {
    let mut distance = vec![u32::MAX; adjacency.len()];
    distance[source as usize] = 0;
    let mut queue = VecDeque::from([source]);
    while let Some(v) = queue.pop_front() {
        for &w in &adjacency[v as usize] {
            if Some(w) != failed && distance[w as usize] == u32::MAX {
                distance[w as usize] = distance[v as usize] + 1;
                queue.push_back(w);
            }
        }
    }
    distance
}

fn assert_path(product: Product, path: &[u32], source: u32, target: u32, failed: Option<u32>) {
    assert_eq!(path.first(), Some(&source));
    assert_eq!(path.last(), Some(&target));
    assert!(path.len() <= 5, "{product:?}: {path:?}");
    let unique: BTreeSet<_> = path.iter().copied().collect();
    assert_eq!(unique.len(), path.len());
    assert!(failed.is_none_or(|f| !unique.contains(&f)));
    for edge in path.windows(2) {
        assert!(product.neighbors(edge[0]).contains(&edge[1]));
    }
}

#[test]
fn factors_exact_degree_diameter_and_deterministic_shortest_next() {
    for code in codes() {
        let factor = Factor::from_code(code).unwrap();
        let expected_degree = match code {
            50 => 7,
            200 => 16,
            n => n - 1,
        };
        assert_eq!(factor.order(), code);
        let adjacency: Vec<_> = (0..code).map(|v| factor.neighbors(v)).collect();
        let mut diameter = 0;
        for s in 0..code {
            let neighbors = &adjacency[s as usize];
            assert_eq!(neighbors.len(), expected_degree as usize);
            assert!(neighbors.windows(2).all(|w| w[0] < w[1]));
            assert!(!neighbors.contains(&s));
            for &t in neighbors {
                assert!(t < code);
                assert!(adjacency[t as usize].contains(&s));
            }
            let oracle = distances(&adjacency, s, None);
            for t in 0..code {
                let d = oracle[t as usize];
                diameter = diameter.max(d);
                assert_eq!(factor.distance(s, t), d);
                let expected_next = if s == t {
                    s
                } else {
                    // BFS from target, independent of the cached distance table.
                    let to_target = distances(&adjacency, t, None);
                    *neighbors
                        .iter()
                        .find(|&&w| to_target[w as usize] + 1 == d)
                        .unwrap()
                };
                assert_eq!(factor.next(s, t), expected_next);
            }
        }
        assert_eq!(
            diameter,
            if code == 1 {
                0
            } else if code <= 32 {
                1
            } else {
                2
            }
        );
    }
}

#[test]
fn every_factor_single_vertex_fault_has_diameter_at_most_four() {
    for code in codes() {
        let factor = Factor::from_code(code).unwrap();
        let adjacency: Vec<_> = (0..code).map(|v| factor.neighbors(v)).collect();
        for failed in 0..code {
            for source in 0..code {
                if source == failed {
                    continue;
                }
                let distance = distances(&adjacency, source, Some(failed));
                for target in 0..code {
                    if target != failed {
                        assert!(
                            distance[target as usize] <= 4,
                            "{factor:?}: {source}->{target}, failed {failed}"
                        );
                    }
                }
            }
        }
    }
}

#[test]
fn fixed_labels_match_the_published_constructions() {
    assert_eq!(
        Factor::HoffmanSingleton.neighbors(0),
        vec![1, 4, 25, 30, 35, 40, 45]
    );
    assert_eq!(
        Factor::HoffmanSingleton.neighbors(7),
        vec![6, 8, 27, 33, 39, 40, 46]
    );
    assert_eq!(
        Factor::Abas.neighbors(0),
        vec![
            1, 11, 19, 21, 27, 35, 64, 79, 82, 100, 101, 105, 138, 156, 159, 171
        ]
    );
}

fn check_healthy_and_prefixes(product: Product, source: u32, target: u32) {
    let path = product.route(source, target).unwrap();
    assert_path(product, &path, source, target, None);
    assert_eq!(path, product.route(source, target).unwrap());
    let n = product.right.order();
    let expected_distance = product.left.distance(source / n, target / n)
        + product.right.distance(source % n, target % n);
    assert_eq!(path.len() as u32 - 1, expected_distance);
    for edge in path.windows(2) {
        let dl = product.left.distance(edge[0] / n, target / n);
        let dr = product.right.distance(edge[0] % n, target % n);
        if dl == 2 {
            assert_ne!(edge[0] / n, edge[1] / n);
        } else if dr == 2 {
            assert_ne!(edge[0] % n, edge[1] % n);
        }
    }
    // Failure is discovered before traversing an internal next hop.
    for failed_index in 1..path.len().saturating_sub(1) {
        let prefix_hops = failed_index - 1;
        let current = path[prefix_hops];
        let failed = path[failed_index];
        let repair = product.repair(current, target, failed).unwrap();
        assert_path(product, &repair, current, target, Some(failed));
        assert!(
            prefix_hops + repair.len() - 1 <= 4,
            "{product:?}: healthy {path:?}, repair {repair:?}"
        );
    }
}

#[test]
fn exhaustive_small_products_repair_is_shortest_and_avoids_failure() {
    for left in 1..=4 {
        for right in 1..=4 {
            let product = Product::new(left, right).unwrap();
            let adjacency: Vec<_> = (0..product.order()).map(|v| product.neighbors(v)).collect();
            for source in 0..product.order() {
                for target in 0..product.order() {
                    check_healthy_and_prefixes(product, source, target);
                }
                for failed in 0..product.order() {
                    if source == failed {
                        continue;
                    }
                    let oracle = distances(&adjacency, source, Some(failed));
                    for target in 0..product.order() {
                        if target == failed {
                            continue;
                        }
                        let path = product.repair(source, target, failed).unwrap();
                        assert_path(product, &path, source, target, Some(failed));
                        assert_eq!(path.len() as u32 - 1, oracle[target as usize]);
                        assert_eq!(path, product.repair(source, target, failed).unwrap());
                    }
                }
            }
        }
    }
}

fn random(state: &mut u64, bound: u32) -> u32 {
    *state = state
        .wrapping_mul(6364136223846793005)
        .wrapping_add(1442695040888963407);
    (*state >> 32) as u32 % bound
}

#[test]
fn all_factor_pairs_and_broad_large_product_prefix_failures() {
    let mut state = 0xface_cafe;
    for left in codes() {
        for right in codes() {
            let product = Product::new(left, right).unwrap();
            assert_eq!(product.codes(), (left, right));
            assert_eq!(product.order(), left * right);
            for _ in 0..4 {
                let source = random(&mut state, product.order());
                let target = random(&mut state, product.order());
                let neighbors = product.neighbors(source);
                assert_eq!(
                    neighbors.len() as u32,
                    product.left.degree() + product.right.degree()
                );
                assert!(neighbors.windows(2).all(|w| w[0] < w[1]));
                for &neighbor in &neighbors {
                    assert!(product.neighbors(neighbor).contains(&source));
                }
                check_healthy_and_prefixes(product, source, target);
            }
        }
    }
    for (left, right) in [
        (1, 50),
        (50, 1),
        (1, 200),
        (200, 1),
        (2, 50),
        (50, 2),
        (2, 200),
        (200, 2),
        (32, 32),
        (50, 50),
        (50, 200),
        (200, 50),
        (200, 200),
    ] {
        let product = Product::new(left, right).unwrap();
        let adjacency: Vec<_> = (0..product.order()).map(|v| product.neighbors(v)).collect();
        for _ in 0..128 {
            let source = random(&mut state, product.order());
            let target = random(&mut state, product.order());
            check_healthy_and_prefixes(product, source, target);
            let failed = random(&mut state, product.order());
            if failed != source && failed != target {
                let path = product.repair(source, target, failed).unwrap();
                assert_path(product, &path, source, target, Some(failed));
                let oracle = distances(&adjacency, source, Some(failed));
                assert_eq!(path.len() as u32 - 1, oracle[target as usize]);
            }
        }
    }
}

#[test]
fn dynamic_order_reconsiders_the_other_distance_two_coordinate() {
    let product = Product::new(50, 200).unwrap();
    let left_target = (0..50).find(|&v| product.left.distance(0, v) == 2).unwrap();
    let right_target = (0..200)
        .find(|&v| product.right.distance(0, v) == 2)
        .unwrap();
    let path = product.route(0, left_target * 200 + right_target).unwrap();
    assert_eq!(path.len(), 5);
    assert_ne!(path[0] / 200, path[1] / 200);
    assert_eq!(path[1] / 200, path[2] / 200);
    assert_ne!(path[1] % 200, path[2] % 200);
    check_healthy_and_prefixes(product, 0, *path.last().unwrap());
}

#[test]
fn selection_matches_independent_cost_ordering_for_small_counts_and_boundaries() {
    // Explicit code/degree catalog avoids relying on production degree arithmetic.
    let factors: Vec<_> = (1..=32)
        .map(|n| (n, n - 1))
        .chain([(50, 7), (200, 16)])
        .collect();
    let mut counts: BTreeSet<_> = (1..=4096).collect();
    counts.extend([9999, 10000, 10001, 39999, 40000, 40001, 99999, 100000]);
    for &(left, _) in &factors {
        for &(right, _) in &factors {
            let base = left * right;
            for multiple in [1, 2, 100000 / base] {
                for n in [base * multiple - 1, base * multiple, base * multiple + 1] {
                    if (1..=100000).contains(&n) {
                        counts.insert(n);
                    }
                }
            }
        }
    }
    for n in counts {
        let mut candidates = Vec::new();
        for &(left, dl) in &factors {
            for &(right, dr) in &factors {
                let base = left * right;
                if base <= n && (base == n || dl + dr >= 2) {
                    let bundles = n / base + u32::from(n % base != 0);
                    candidates.push((bundles * (dl + dr), std::cmp::Reverse(base), left, right));
                }
            }
        }
        let &(_, _, left, right) = candidates.iter().min().unwrap();
        assert_eq!(choose(n).unwrap().codes(), (left, right), "N={n}");
    }
    assert_eq!(choose(1).unwrap().codes(), (1, 1));
    assert_eq!(choose(2).unwrap().codes(), (1, 2));
    assert_eq!(choose(3).unwrap().codes(), (1, 3));
    let exact = choose(10000).unwrap();
    assert_eq!(exact.codes(), (50, 200));
    assert_eq!(exact.left.degree() + exact.right.degree(), 23);
    for n in [0, 100001, u32::MAX] {
        assert!(choose(n).is_err());
    }
}

#[test]
fn independent_bundles_survive_one_physical_intermediate_failure() {
    for (left, right) in [(1, 1), (1, 2), (2, 1)] {
        let product = Product::new(left, right).unwrap();
        assert!(product.supports_members(product.order()));
        for n in product.order() + 1..=8 {
            assert!(!product.supports_members(n));
        }
    }
    for n in 1..=50 {
        let product = choose(n).unwrap();
        assert!(product.supports_members(n));
        let roles: Vec<_> = (0..n).map(|i| i % product.order()).collect();
        let adjacency: Vec<Vec<u32>> = roles
            .iter()
            .map(|&role| {
                let neighbors = product.neighbors(role);
                roles
                    .iter()
                    .enumerate()
                    .filter_map(|(i, r)| neighbors.contains(r).then_some(i as u32))
                    .collect()
            })
            .collect();
        for source in 0..n {
            for failed in 0..n {
                if source == failed {
                    continue;
                }
                let oracle = distances(&adjacency, source, Some(failed));
                for target in 0..n {
                    if target != failed {
                        assert!(
                            oracle[target as usize] <= 4,
                            "N={n}: {source}->{target}, failed {failed}"
                        );
                    }
                }
            }
        }
    }
    assert!(!Product::new(1, 3).unwrap().supports_members(0));
    assert!(!Product::new(1, 3).unwrap().supports_members(100_001));
}

#[test]
fn malformed_codes_roles_factors_and_failed_endpoints_are_rejected() {
    for bad in [0, 33, 49, 51, 199, 201, u32::MAX] {
        assert!(Product::new(bad, 1).is_err());
        assert!(Product::new(1, bad).is_err());
    }
    let product = Product::new(50, 200).unwrap();
    for bad in [product.order(), u32::MAX] {
        assert!(product.route(bad, 0).is_err());
        assert!(product.route(0, bad).is_err());
        assert!(product.repair(bad, 0, 1).is_err());
        assert!(product.repair(0, bad, 1).is_err());
        assert!(product.repair(0, 0, bad).is_err());
        assert!(std::panic::catch_unwind(|| product.neighbors(bad)).is_err());
    }
    assert_eq!(product.route(7, 7).unwrap(), vec![7]);
    assert_eq!(product.repair(7, 7, 8).unwrap(), vec![7]);
    assert!(product.repair(7, 7, 7).is_err());
    assert!(product.repair(7, 8, 7).is_err());
    assert!(product.repair(7, 8, 8).is_err());
    assert_eq!(Product::new(1, 1).unwrap().route(0, 0).unwrap(), vec![0]);
    assert!(Product::new(1, 1).unwrap().repair(0, 0, 0).is_err());
    for invalid in [0, 33, u32::MAX] {
        let malformed = Product {
            left: Factor::Clique(invalid),
            right: Factor::Clique(1),
        };
        assert!(malformed.route(0, 0).is_err());
        assert!(malformed.repair(0, 0, 0).is_err());
        assert!(std::panic::catch_unwind(|| malformed.order()).is_err());
    }
    for code in codes() {
        let factor = Factor::from_code(code).unwrap();
        assert!(std::panic::catch_unwind(|| factor.neighbors(code)).is_err());
        assert!(std::panic::catch_unwind(|| factor.distance(code, 0)).is_err());
        assert!(std::panic::catch_unwind(|| factor.next(0, code)).is_err());
    }
}
